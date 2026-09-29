package taskservice

import (
	"sort"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// TaskView is what the API shows of a task. It leaves out the token and
// local paths.
type TaskView struct {
	TaskID          string          `json:"task_id"`
	Attempt         int             `json:"attempt"`
	Profile         string          `json:"profile"`
	Channel         string          `json:"channel,omitempty"`
	Title           string          `json:"title"`
	Visibility      string          `json:"visibility"`
	Status          contract.Status `json:"status"`
	Step            string          `json:"step,omitempty"`
	Progress        int             `json:"progress"`
	Message         string          `json:"message,omitempty"`
	VideoID         string          `json:"video_id,omitempty"`
	VideoURL        string          `json:"video_url,omitempty"`
	ExistingVideoID string          `json:"existing_video_id,omitempty"`
	// QueuePosition is 1 for the next task of the profile, 0 when not queued.
	QueuePosition int `json:"queue_position"`
	// Kind is upload or check_video; ParentID is the upload a check reads.
	Kind       string               `json:"kind,omitempty"`
	ParentID   string               `json:"parent_id,omitempty"`
	VideoState *contract.VideoState `json:"video_state,omitempty"`
	// AgentID is the launcher the current attempt was assigned to.
	AgentID   string             `json:"agent_id,omitempty"`
	ErrorCode contract.ErrorCode `json:"error_code,omitempty"`
	Error     string             `json:"error,omitempty"`
	// FinishReported is true once the agent called task_finish for the
	// current attempt; Finish holds what it reported.
	FinishReported bool              `json:"finish_reported"`
	Finish         *task.FinishInfo  `json:"finish,omitempty"`
	SessionEnded   bool              `json:"session_ended"`
	Session        *task.SessionInfo `json:"session,omitempty"`
	LastEvent      *task.Event       `json:"last_event,omitempty"`
	Events         []task.Event      `json:"events,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// viewLocked copies t for the API; detail adds the whole timeline.
func (s *Service) viewLocked(t *task.Task, detail bool) TaskView {
	v := TaskView{
		TaskID: t.ID(), Attempt: t.Attempt(),
		Profile: t.Profile(), Channel: t.Spec.ChannelID,
		Status: t.Status, Step: t.Step, Progress: t.Progress, Message: t.Message,
		VideoID: t.VideoID, VideoURL: t.VideoURL, ExistingVideoID: t.Spec.ExistingVideoID,
		AgentID: t.AgentID, Kind: t.Spec.Kind, ParentID: t.ParentID, VideoState: t.VideoState,
		ErrorCode: t.ErrorCode, Error: t.Error,
		FinishReported: t.Finish != nil, Finish: t.Finish,
		SessionEnded: t.Session != nil, Session: t.Session,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		QueuePosition: task.Position(s.all(), t),
	}
	if m := t.Spec.Metadata; m != nil {
		v.Title, v.Visibility = m.Title, string(m.Visibility)
	}
	if n := len(t.Events); n > 0 {
		last := t.Events[n-1]
		v.LastEvent = &last
	}
	if detail {
		v.Events = append([]task.Event(nil), t.Events...)
	}
	return v
}

// AgentInfo is what a connected launcher announced.
type AgentInfo struct {
	AgentID     string                  `json:"agent_id"`
	Version     string                  `json:"version"`
	Profiles    []contract.ProfileState `json:"profiles"`
	ConnectedAt time.Time               `json:"connected_at"`
	LastSeen    time.Time               `json:"last_seen"`
}

// QueueView is one profile in GET /queues.
type QueueView struct {
	Profile    string   `json:"profile"`
	Running    string   `json:"running,omitempty"`
	Queued     []string `json:"queued"`
	Subscribed bool     `json:"subscribed"`
	AgentID    string   `json:"agent_id,omitempty"`
}

// VideoView is GET /tasks/:id/video and a row of GET /videos.
type VideoView struct {
	TaskID     string               `json:"task_id"`
	Profile    string               `json:"profile"`
	Title      string               `json:"title"`
	Status     contract.Status      `json:"status"`
	VideoID    string               `json:"video_id,omitempty"`
	VideoURL   string               `json:"video_url,omitempty"`
	VideoState *contract.VideoState `json:"video_state,omitempty"`
	Rechecks   int                  `json:"rechecks"`
	Checks     []TaskView           `json:"checks,omitempty"`
}

func (s *Service) videoViewLocked(t *task.Task, withChecks bool) VideoView {
	v := VideoView{
		TaskID: t.ID(), Profile: t.Profile(), Title: t.Title(), Status: t.Status,
		VideoID: t.VideoID, VideoURL: t.VideoURL, VideoState: t.VideoState, Rechecks: t.Rechecks,
	}
	if withChecks {
		for _, c := range s.byID {
			if c.ParentID == t.ID() {
				v.Checks = append(v.Checks, s.viewLocked(c, false))
			}
		}
		sort.Slice(v.Checks, func(a, b int) bool { return v.Checks[a].CreatedAt.Before(v.Checks[b].CreatedAt) })
	}
	return v
}
