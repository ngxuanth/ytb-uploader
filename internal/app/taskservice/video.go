package taskservice

import (
	"log"
	"sort"
	"strconv"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// A video's state in Studio (visibility, processing, restrictions) comes in
// through task_video_state: the launcher's script reads it right after an
// upload, and a check_video task reads it again later. Check tasks are
// ordinary queued tasks of the video's profile (no file, no LLM) whose
// ParentID is the upload task; their result is stored on the upload task.

// VideoState is task_video_state.
func (s *Service) VideoState(id string, vs contract.VideoState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.taskLocked(id)
	if err != nil {
		return false, err
	}
	target := t
	if t.ParentID != "" {
		if p := s.byID[t.ParentID]; p != nil {
			target = p
		}
	}
	msg := target.SetVideoState(vs)
	videoID := target.VideoState.VideoID
	t.AddEvent(task.Event{Event: "task_video_state", VideoID: videoID, Message: msg})
	if target != t {
		target.AddEvent(task.Event{Event: "video_checked", VideoID: videoID, Message: msg + " (by " + t.ID() + ")"})
	}
	log.Printf("task_video_state %s video %s: %s", t.ID(), videoID, msg)
	if target.VideoState.Processing {
		s.recheckLaterLocked(target)
	}
	s.saveLocked()
	return t.Stop, nil
}

// newCheckLocked queues a check_video task for the upload's video.
func (s *Service) newCheckLocked(parent *task.Task, reason string) *task.Task {
	now := time.Now()
	c := &task.Task{
		Token: randHex(18),
		Spec: task.Spec{
			Kind: task.KindCheckVideo, TaskID: "chk-" + randHex(4), Attempt: 1,
			ChannelID: parent.Spec.ChannelID, ProfileDirectory: parent.Profile(),
			ExistingVideoID: parent.VideoID,
		},
		ParentID:  parent.ID(),
		Status:    contract.StatusQueued,
		CreatedAt: now, UpdatedAt: now,
	}
	s.add(c)
	task.Enqueue(s.all(), c, false)
	c.AddEvent(task.Event{Event: "queued", VideoID: parent.VideoID, Message: reason})
	parent.AddEvent(task.Event{Event: "check_queued", VideoID: parent.VideoID, Message: c.ID() + ": " + reason})
	log.Printf("queued %s: check video %s of %s (%s)", c.ID(), parent.VideoID, parent.ID(), reason)
	return c
}

// pendingCheckLocked is a check of parent that has not ended yet.
func (s *Service) pendingCheckLocked(parent *task.Task) *task.Task {
	for _, t := range s.byID {
		if t.ParentID == parent.ID() && !t.Ended() {
			return t
		}
	}
	return nil
}

// recheckLaterLocked schedules a check of a video Studio still processes.
func (s *Service) recheckLaterLocked(parent *task.Task) {
	if s.cfg.Recheck <= 0 || parent.Rechecks >= maxRechecks {
		return
	}
	id := parent.ID()
	time.AfterFunc(s.cfg.Recheck, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.byID[id]
		if p == nil || p.VideoState == nil || !p.VideoState.Processing || p.Rechecks >= maxRechecks || s.pendingCheckLocked(p) != nil {
			return
		}
		p.Rechecks++
		s.newCheckLocked(p, "still processing, recheck "+strconv.Itoa(p.Rechecks))
		s.dispatchLocked()
	})
}

// Video is the last state read for the upload's video and its checks. id
// may be the upload task or one of its checks.
func (s *Service) Video(id string) (VideoView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	if t == nil {
		return VideoView{}, notFound("no such task")
	}
	if t.ParentID != "" {
		if t = s.byID[t.ParentID]; t == nil {
			return VideoView{}, notFound("the checked upload task is gone")
		}
	}
	return s.videoViewLocked(t, true), nil
}

// CheckVideo queues a check of the upload's video (or returns the pending
// one).
func (s *Service) CheckVideo(id string) (TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	switch {
	case t == nil:
		return TaskView{}, notFound("no such task")
	case t.ParentID != "":
		return TaskView{}, conflict("this is a check task; check its upload task " + t.ParentID)
	case t.VideoID == "":
		return TaskView{}, conflict("the task has not created a video yet")
	}
	if p := s.pendingCheckLocked(t); p != nil {
		return s.viewLocked(p, true), nil
	}
	c := s.newCheckLocked(t, "requested")
	s.dispatchLocked()
	return s.viewLocked(c, true), nil
}

// Videos is every uploaded video (optionally of one profile) and its state.
func (s *Service) Videos(profile string) []VideoView {
	s.mu.Lock()
	out := []VideoView{}
	for _, t := range s.byID {
		if t.ParentID != "" || t.VideoID == "" || (profile != "" && t.Profile() != profile) {
			continue
		}
		out = append(out, s.videoViewLocked(t, false))
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].TaskID < out[b].TaskID })
	return out
}
