package main

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/gofiber/fiber/v3"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// A video's state in Studio (visibility, processing, restrictions) comes in
// through task_video_state: the launcher's script reads it right after an
// upload, and a check_video task reads it again later. Check tasks are
// ordinary queued tasks of the video's profile (no file, no LLM) whose
// ParentID is the upload task; their result is stored on the upload task.

// maxRechecks bounds the automatic checks of a video still processing.
const maxRechecks = 6

// VideoState records what the script read about the task's video.
func (st *state) VideoState(_ context.Context, s *taskmcp.Session, in taskmcp.VideoStateIn) (*taskmcp.Ack, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.jobOf(s)
	if err != nil {
		return nil, err
	}
	vs := in.VideoState
	if vs.CheckedAt.IsZero() {
		vs.CheckedAt = time.Now()
	}
	target := j
	if j.ParentID != "" {
		if p := st.byID[j.ParentID]; p != nil {
			target = p
		}
	}
	if vs.VideoID == "" {
		vs.VideoID = target.VideoID
	}
	target.VideoState = &vs
	msg := vs.Visibility
	if vs.Processing {
		msg += ", processing"
	}
	if vs.Restrictions != "" {
		msg += ", restrictions: " + vs.Restrictions
	}
	j.addEvent(eventEntry{Event: "task_video_state", VideoID: vs.VideoID, Message: msg})
	if target != j {
		target.addEvent(eventEntry{Event: "video_checked", VideoID: vs.VideoID, Message: msg + " (by " + j.Claim.TaskID + ")"})
	}
	log.Printf("task_video_state %s video %s: %s", j.Claim.TaskID, vs.VideoID, msg)
	if vs.Processing {
		st.recheckLaterLocked(target)
	}
	st.saveLocked()
	return st.ackLocked(j), nil
}

// newCheckLocked queues a check_video task for the upload task's video.
func (st *state) newCheckLocked(parent *job, reason string) *job {
	now := time.Now()
	c := &job{
		Token: randHex(18),
		Claim: &taskmcp.ClaimOut{
			Control: taskmcp.Continue, Kind: taskmcp.KindCheckVideo,
			TaskID: "chk-" + randHex(4), Attempt: 1,
			ChannelID: parent.Claim.ChannelID, ProfileDirectory: parent.Claim.ProfileDirectory,
			ExistingVideoID: parent.VideoID,
		},
		ParentID:  parent.Claim.TaskID,
		Status:    contract.StatusQueued,
		CreatedAt: now, UpdatedAt: now,
	}
	st.byID[c.Claim.TaskID] = c
	st.byToken[c.Token] = c
	st.enqueueLocked(c, false)
	c.addEvent(eventEntry{Event: "queued", VideoID: parent.VideoID, Message: reason})
	parent.addEvent(eventEntry{Event: "check_queued", VideoID: parent.VideoID, Message: c.Claim.TaskID + ": " + reason})
	log.Printf("queued %s: check video %s of %s (%s)", c.Claim.TaskID, parent.VideoID, parent.Claim.TaskID, reason)
	return c
}

// pendingCheckLocked is a check of parent that has not ended yet.
func (st *state) pendingCheckLocked(parent *job) *job {
	for _, j := range st.byID {
		if j.ParentID == parent.Claim.TaskID && !j.Status.Terminal() && j.Status != contract.StatusLost && j.Status != contract.StatusNeedsAttention {
			return j
		}
	}
	return nil
}

// recheckLaterLocked schedules a check of a video Studio still processes.
func (st *state) recheckLaterLocked(parent *job) {
	if st.recheck <= 0 || parent.Rechecks >= maxRechecks {
		return
	}
	id := parent.Claim.TaskID
	time.AfterFunc(st.recheck, func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		p := st.byID[id]
		if p == nil || p.VideoState == nil || !p.VideoState.Processing || p.Rechecks >= maxRechecks || st.pendingCheckLocked(p) != nil {
			return
		}
		p.Rechecks++
		st.newCheckLocked(p, "still processing, recheck "+itoa(p.Rechecks))
		st.dispatchLocked()
	})
}

// videoView is GET /tasks/:id/video.
type videoView struct {
	TaskID     string               `json:"task_id"`
	Profile    string               `json:"profile"`
	Title      string               `json:"title"`
	Status     contract.Status      `json:"status"`
	VideoID    string               `json:"video_id,omitempty"`
	VideoURL   string               `json:"video_url,omitempty"`
	VideoState *contract.VideoState `json:"video_state,omitempty"`
	Rechecks   int                  `json:"rechecks"`
	Checks     []taskView           `json:"checks,omitempty"`
}

func (st *state) videoViewLocked(j *job, withChecks bool) videoView {
	v := videoView{
		TaskID: j.Claim.TaskID, Profile: j.Claim.ProfileDirectory, Status: j.Status,
		VideoID: j.VideoID, VideoURL: j.VideoURL, VideoState: j.VideoState, Rechecks: j.Rechecks,
	}
	if m := j.Claim.Metadata; m != nil {
		v.Title = m.Title
	}
	if withChecks {
		for _, c := range st.byID {
			if c.ParentID == j.Claim.TaskID {
				v.Checks = append(v.Checks, st.viewLocked(c, false))
			}
		}
		sort.Slice(v.Checks, func(a, b int) bool { return v.Checks[a].CreatedAt.Before(v.Checks[b].CreatedAt) })
	}
	return v
}

// getVideo is GET /tasks/:id/video: the last state read and the checks.
func (st *state) getVideo(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.byID[c.Params("id")]
	if j == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such task"})
	}
	if j.ParentID != "" {
		j = st.byID[j.ParentID]
		if j == nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "the checked upload task is gone"})
		}
	}
	return c.JSON(st.videoViewLocked(j, true))
}

// checkVideo is POST /tasks/:id/video/check: queue a check of the video.
func (st *state) checkVideo(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.byID[c.Params("id")]
	if j == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such task"})
	}
	if j.ParentID != "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "this is a check task; check its upload task " + j.ParentID})
	}
	if j.VideoID == "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "the task has not created a video yet"})
	}
	if p := st.pendingCheckLocked(j); p != nil {
		return c.Status(fiber.StatusAccepted).JSON(st.viewLocked(p, true))
	}
	chk := st.newCheckLocked(j, "requested")
	st.dispatchLocked()
	return c.Status(fiber.StatusAccepted).JSON(st.viewLocked(chk, true))
}

// listVideos is GET /videos?profile=: every uploaded video and its state.
func (st *state) listVideos(c fiber.Ctx) error {
	profile := c.Query("profile")
	st.mu.Lock()
	out := []videoView{}
	for _, j := range st.byID {
		if j.ParentID != "" || j.VideoID == "" {
			continue
		}
		if profile != "" && j.Claim.ProfileDirectory != profile {
			continue
		}
		out = append(out, st.videoViewLocked(j, false))
	}
	st.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].TaskID < out[b].TaskID })
	return c.JSON(fiber.Map{"videos": out})
}
