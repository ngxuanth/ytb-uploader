package taskservice

import (
	"log"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// The calls an agent session makes (through task_mcp / report_mcp). Each
// returns stop=true once the attempt must end (finished, cancelled, lost).

// Authenticate resolves a session token to its task.
func (s *Service) Authenticate(token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byToken[token]
	if t == nil {
		return "", ErrUnauthorized
	}
	return t.ID(), nil
}

func (s *Service) taskLocked(id string) (*task.Task, error) {
	t := s.byID[id]
	if t == nil {
		return nil, ErrUnauthorized
	}
	return t, nil
}

// Claim is task_claim.
func (s *Service) Claim(id string) (spec task.Spec, stop bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.taskLocked(id)
	if err != nil {
		return task.Spec{}, false, err
	}
	t.AddEvent(task.Event{Event: "task_claim"})
	s.saveLocked()
	if t.Stop {
		return task.Spec{}, true, nil
	}
	log.Printf("task_claim -> %s attempt %d", t.ID(), t.Attempt())
	return t.Claimed(), false, nil
}

// Report is task_report.
func (s *Service) Report(id, step string, progress int, message string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.taskLocked(id)
	if err != nil {
		return false, err
	}
	log.Printf("task_report %s %s %d%% %s", t.ID(), step, progress, message)
	t.Report(step, message, progress)
	s.saveLocked()
	return t.Stop, nil
}

// VideoCreated is task_video_created.
func (s *Service) VideoCreated(id, videoID, videoURL string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.taskLocked(id)
	if err != nil {
		return false, err
	}
	log.Printf("task_video_created %s %s %s", t.ID(), videoID, videoURL)
	t.VideoCreated(videoID, videoURL)
	s.saveLocked()
	return t.Stop, nil
}

// Finish is task_finish; the attempt always stops.
func (s *Service) Finish(id string, f task.FinishInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.taskLocked(id)
	if err != nil {
		return err
	}
	log.Printf("task_finish %s %s video=%s code=%s reason=%s", t.ID(), f.Status, f.VideoID, f.ErrorCode, f.Reason)
	t.Finished(f)
	s.saveLocked()
	s.stopAfterGraceLocked(t)
	return nil
}

// stopAfterGraceLocked asks the launcher to end t's session if it is still
// running FinishGrace after task_finish. Weak models keep calling tools after
// "stop", and the profile's queue waits for session_ended.
func (s *Service) stopAfterGraceLocked(t *task.Task) {
	grace := s.cfg.FinishGrace
	if grace <= 0 || !t.Holding {
		return
	}
	id, attempt := t.ID(), t.Attempt()
	time.AfterFunc(grace, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.byID[id]
		if t == nil || t.Attempt() != attempt || !t.Holding || t.Session != nil {
			return
		}
		msg := "session still running " + grace.String() + " after task_finish; asked launcher to stop it"
		if err := s.sendToLocked(t.AgentID, contract.MsgCancel, contract.Cancel{TaskRef: contract.TaskRef{TaskID: id, Attempt: attempt}}); err != nil {
			msg = "session still running after task_finish; launcher not told: " + err.Error()
		}
		t.AddEvent(task.Event{Event: "stop_after_finish", Message: msg})
		log.Printf("%s: %s", id, msg)
		s.saveLocked()
	})
}
