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
	TaskID          string      `json:"task_id"`
	Attempt         int         `json:"attempt"`
	Profile         string      `json:"profile"`
	Channel         string      `json:"channel,omitempty"`
	Title           string      `json:"title"`
	Visibility      string      `json:"visibility"`
	Status          wire.Status `json:"status"`
	Step            string      `json:"step,omitempty"`
	Progress        int         `json:"progress"`
	Message         string      `json:"message,omitempty"`
	VideoID         string      `json:"video_id,omitempty"`
	VideoURL        string      `json:"video_url,omitempty"`
	ExistingVideoID string      `json:"existing_video_id,omitempty"`
	// QueuePosition is 1 for the next task of the profile, 0 when not queued.
	QueuePosition int `json:"queue_position"`
	// AgentID is the launcher the current attempt was assigned to.
	AgentID   string         `json:"agent_id,omitempty"`
	ErrorCode wire.ErrorCode `json:"error_code,omitempty"`
	Error     string         `json:"error,omitempty"`
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
		AgentID:   j.AgentID,
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

// viewLocked is j.view plus what depends on the other jobs.
func (st *state) viewLocked(j *job, detail bool) taskView {
	v := j.view(detail)
	v.QueuePosition = st.queuePositionLocked(j)
	return v
}

// agentHandler lists the connected launchers; "agent" is the newest one.
func (st *state) agentHandler(c fiber.Ctx) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	agents := []agentInfo{}
	var newest *agentInfo
	for _, a := range st.agents {
		agents = append(agents, a.info)
	}
	sort.Slice(agents, func(a, b int) bool { return agents[a].ConnectedAt.After(agents[b].ConnectedAt) })
	if len(agents) > 0 {
		newest = &agents[0]
	}
	return c.JSON(fiber.Map{"connected": len(agents) > 0, "agent": newest, "agents": agents})
}

// listQueues shows, per profile, the running task, the waiting ones in
// order, and whether a launcher serves the profile.
func (st *state) listQueues(c fiber.Ctx) error {
	names, _ := st.profileNames()
	st.mu.Lock()
	defer st.mu.Unlock()
	return c.JSON(fiber.Map{"queues": st.queuesLocked(names)})
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
		out = append(out, st.viewLocked(j, false))
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
	return c.JSON(st.viewLocked(j, true))
}

// retryTask queues a new attempt of a failed, lost, cancelled or
// needs-attention task, at the end of its profile's queue or, with
// ?front=true, ahead of the waiting tasks. If an earlier attempt created the
// video, the new one gets existing_video_id and finishes that video instead
// of uploading again.
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
	if j.Holding {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "the previous attempt's session is still running; retry after session_ended",
		})
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
	j.AgentID = ""
	front := c.Query("front") == "true" || c.Query("front") == "1"
	st.enqueueLocked(j, front)
	j.addEvent(eventEntry{Event: "retry", VideoID: j.Claim.ExistingVideoID})
	log.Printf("retry %s attempt %d existing_video=%s front=%v", j.Claim.TaskID, j.Claim.Attempt, j.Claim.ExistingVideoID, front)
	st.dispatchLocked()
	return c.Status(fiber.StatusAccepted).JSON(st.viewLocked(j, true))
}

// cancelTask stops a task. A queued one just leaves the queue. A running one
// answers "stop" to its next task_* call and its launcher is told to kill the
// session; the profile is freed when session_ended arrives, or right away if
// that launcher is not connected.
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
	wasQueued := j.Status == wire.StatusQueued
	j.Stop = true
	j.Status = wire.StatusCancelled
	msg := ""
	switch {
	case wasQueued:
		msg = "removed from the queue"
	case j.Holding:
		if err := st.sendToLocked(j.AgentID, wire.MsgCancel, wire.Cancel{TaskRef: wire.TaskRef{TaskID: j.Claim.TaskID, Attempt: j.Claim.Attempt}}); err != nil {
			msg = "agent not told: " + err.Error()
			st.releaseLocked(j)
		}
	}
	j.addEvent(eventEntry{Event: "cancel", Message: msg})
	log.Printf("cancel %s attempt %d %s", j.Claim.TaskID, j.Claim.Attempt, msg)
	st.dispatchLocked()
	return c.JSON(st.viewLocked(j, true))
}

func itoa(n int) string { return strconv.Itoa(n) }
