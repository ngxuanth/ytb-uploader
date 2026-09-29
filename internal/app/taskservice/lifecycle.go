package taskservice

import (
	"log"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// Rejected records that a launcher refused an attempt; the profile is free.
func (s *Service) Rejected(rej contract.Reject) {
	log.Printf("agent rejected %s: %s", rej.TaskID, rej.Reason)
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.attemptLocked(rej.TaskRef)
	if t == nil {
		return
	}
	t.Rejected(rej.Reason)
	s.dispatchLocked()
}

// SessionEnded records that an attempt's harness exited. It frees the
// profile, so the profile's next task starts.
func (s *Service) SessionEnded(ev contract.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.attemptLocked(ev.TaskRef)
	if t == nil {
		return
	}
	msg := t.SessionEnded(sessionInfo(ev))
	log.Printf("session ended %s attempt %d: %s, finish_reported=%v", t.ID(), t.Attempt(), msg, t.Finish != nil)
	s.dispatchLocked()
}

// sessionInfo reads the session_ended event the launcher sends.
func sessionInfo(ev contract.Event) task.SessionInfo {
	si := task.SessionInfo{EndedAt: ev.At}
	if v, ok := ev.Data["exit_code"].(float64); ok {
		si.ExitCode = int(v)
	}
	if v, ok := ev.Data["duration_ms"].(float64); ok {
		si.DurationMS = int64(v)
	}
	si.KilledBy, _ = ev.Data["killed_by"].(string)
	si.Error, _ = ev.Data["error"].(string)
	si.Runner, _ = ev.Data["runner"].(string)
	if v, ok := ev.Data["handoffs"].(float64); ok {
		si.Handoffs = int(v)
	}
	if v, ok := ev.Data["failed_steps"].([]any); ok {
		for _, x := range v {
			if name, ok := x.(string); ok {
				si.FailedSteps = append(si.FailedSteps, name)
			}
		}
	}
	return si
}

// Retry queues a new attempt of a failed, lost, cancelled or needs-attention
// task, at the end of its profile's queue or (front) ahead of the waiting
// tasks. If an earlier attempt created the video, the new one gets
// existing_video_id and finishes that video instead of uploading again.
func (s *Service) Retry(id string, front bool) (TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	if t == nil {
		return TaskView{}, notFound("no such task")
	}
	oldToken := t.Token
	switch err := t.NewAttempt(randHex(18)); err {
	case nil:
	case task.ErrNotRetryable:
		return TaskView{}, conflict("task is " + string(t.Status) + "; only FAILED, CANCELLED, LOST or NEEDS_ATTENTION can be retried")
	default:
		return TaskView{}, conflict(err.Error())
	}
	delete(s.byToken, oldToken)
	s.byToken[t.Token] = t
	task.Enqueue(s.all(), t, front)
	t.AddEvent(task.Event{Event: "retry", VideoID: t.Spec.ExistingVideoID})
	log.Printf("retry %s attempt %d existing_video=%s front=%v", t.ID(), t.Attempt(), t.Spec.ExistingVideoID, front)
	s.dispatchLocked()
	return s.viewLocked(t, true), nil
}

// Cancel stops a task. A queued one just leaves the queue. A running one
// answers "stop" to its next task_* call and its launcher is told to kill the
// session; the profile is freed when session_ended arrives, or right away if
// that launcher is not connected.
func (s *Service) Cancel(id string) (TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	if t == nil {
		return TaskView{}, notFound("no such task")
	}
	wasQueued, err := t.Cancel()
	if err != nil {
		return TaskView{}, conflict("task is already " + string(t.Status))
	}
	msg := ""
	switch {
	case wasQueued:
		msg = "removed from the queue"
	case t.Holding:
		if err := s.sendToLocked(t.AgentID, contract.MsgCancel, contract.Cancel{TaskRef: contract.TaskRef{TaskID: t.ID(), Attempt: t.Attempt()}}); err != nil {
			msg = "agent not told: " + err.Error()
			t.Holding = false
		}
	}
	t.AddEvent(task.Event{Event: "cancel", Message: msg})
	log.Printf("cancel %s attempt %d %s", t.ID(), t.Attempt(), msg)
	s.dispatchLocked()
	return s.viewLocked(t, true), nil
}
