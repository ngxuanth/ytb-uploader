package main

import (
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// taskView is what the REST API returns for a task. It leaves out the token
// and local paths.
type taskView struct {
	TaskID          string         `json:"task_id"`
	Attempt         int            `json:"attempt"`
	Profile         string         `json:"profile"`
	Channel         string         `json:"channel"`
	Title           string         `json:"title"`
	Visibility      string         `json:"visibility"`
	Status          wire.Status    `json:"status"`
	Step            string         `json:"step,omitempty"`
	Progress        int            `json:"progress"`
	Message         string         `json:"message,omitempty"`
	VideoID         string         `json:"video_id,omitempty"`
	VideoURL        string         `json:"video_url,omitempty"`
	ExistingVideoID string         `json:"existing_video_id,omitempty"`
	ErrorCode       wire.ErrorCode `json:"error_code,omitempty"`
	Error           string         `json:"error,omitempty"`
	// FinishReported is true once the agent called task_finish for the
	// current attempt; Finish holds what it reported.
	FinishReported bool         `json:"finish_reported"`
	Finish         *finishInfo  `json:"finish,omitempty"`
	SessionEnded   bool         `json:"session_ended"`
	Session        *sessionInfo `json:"session,omitempty"`
	LastEvent      *eventEntry  `json:"last_event,omitempty"`
	Events         []eventEntry `json:"events,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// view copies the job for the API; detail adds the whole timeline.
func (j *job) view(detail bool) taskView {
	v := taskView{
		TaskID: j.Claim.TaskID, Attempt: j.Claim.Attempt,
		Profile: j.Claim.ProfileDirectory, Channel: j.Claim.ChannelID,
		Status: j.Status, Step: j.Step, Progress: j.Progress, Message: j.Message,
		VideoID: j.VideoID, VideoURL: j.VideoURL, ExistingVideoID: j.Claim.ExistingVideoID,
		ErrorCode: j.ErrorCode, Error: j.Error,
		FinishReported: j.Finish != nil, Finish: j.Finish,
		SessionEnded: j.Session != nil, Session: j.Session,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
	}
	if m := j.Claim.Metadata; m != nil {
		v.Title, v.Visibility = m.Title, string(m.Visibility)
	}
	if n := len(j.Events); n > 0 {
		last := j.Events[n-1]
		v.LastEvent = &last
	}
	if detail {
		v.Events = append([]eventEntry(nil), j.Events...)
	}
	return v
}

func (st *state) agentHandler(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return c.JSON(fiber.Map{"connected": st.conn != nil, "agent": st.agent})
}

// listTasks returns the tasks, newest first. ?status=FAILED,LOST and
// ?profile=NAME filter them.
func (st *state) listTasks(c fiber.Ctx) error {
	var statuses []wire.Status
	for _, s := range strings.Split(c.Query("status"), ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			statuses = append(statuses, wire.Status(s))
		}
	}
	profile := c.Query("profile")
	st.mu.Lock()
	out := []taskView{}
	for _, j := range st.byID {
		if len(statuses) > 0 && !j.Status.In(statuses) {
			continue
		}
		if profile != "" && j.Claim.ProfileDirectory != profile {
			continue
		}
		out = append(out, j.view(false))
	}
	st.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return c.JSON(fiber.Map{"tasks": out})
}

func (st *state) getTask(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.byID[c.Params("id")]
	if j == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such task"})
	}
	return c.JSON(j.view(true))
}

// retryTask starts a new attempt of a failed, lost, cancelled or
// needs-attention task. If an earlier attempt created the video, the new one
// gets existing_video_id and finishes that video instead of uploading again.
func (st *state) retryTask(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.byID[c.Params("id")]
	if j == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such task"})
	}
	if !j.Status.In(wire.RetryableFrom) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "task is " + string(j.Status) + "; only FAILED, CANCELLED, LOST or NEEDS_ATTENTION can be retried",
		})
	}
	if st.conn == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent is not connected"})
	}
	// The old attempt's token stops working, so a leftover session cannot
	// report into the new attempt.
	delete(st.byToken, j.Token)
	j.Token = randHex(18)
	st.byToken[j.Token] = j
	j.Claim.Attempt++
	if j.VideoID != "" {
		j.Claim.ExistingVideoID = j.VideoID
	}
	j.Stop, j.Finish, j.Session = false, nil, nil
	j.Step, j.Progress, j.Message, j.ErrorCode, j.Error = "", 0, "", "", ""
	j.addEvent(eventEntry{Event: "retry", VideoID: j.Claim.ExistingVideoID})
	if err := st.assignLocked(j); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent disconnected", "task": j.view(true)})
	}
	log.Printf("retry %s attempt %d existing_video=%s", j.Claim.TaskID, j.Claim.Attempt, j.Claim.ExistingVideoID)
	return c.Status(fiber.StatusAccepted).JSON(j.view(true))
}

// cancelTask stops a task: its next task_* call answers "stop" and the
// launcher is told to kill the session.
func (st *state) cancelTask(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.byID[c.Params("id")]
	if j == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such task"})
	}
	if j.Status.Terminal() {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "task is already " + string(j.Status)})
	}
	j.Stop = true
	j.Status = wire.StatusCancelled
	msg := ""
	if err := st.sendLocked(wire.MsgCancel, wire.Cancel{TaskRef: wire.TaskRef{TaskID: j.Claim.TaskID, Attempt: j.Claim.Attempt}}); err != nil {
		msg = "agent not told: " + err.Error()
	}
	j.addEvent(eventEntry{Event: "cancel", Message: msg})
	st.saveLocked()
	log.Printf("cancel %s attempt %d %s", j.Claim.TaskID, j.Claim.Attempt, msg)
	return c.JSON(j.view(true))
}

func itoa(n int) string { return strconv.Itoa(n) }
