// Package task is the domain of the upload server: a task, its attempts and
// timeline, how the agent's reports change it, and the per-profile queue.
// It has no I/O; the application layer (app/taskservice) loads, saves and
// sends.
package task

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Kinds of task.
const (
	KindUpload = "upload"
	// KindCheckVideo reads a video's state in Studio; it has no file and its
	// ParentID is the upload task.
	KindCheckVideo = "check_video"
)

// MaxEvents caps a task's timeline; the oldest lines are dropped first.
const MaxEvents = 200

// Spec is what the agent is asked to do. Its JSON matches task_claim's
// output, and it is stored under "claim" so older task files still load.
type Spec struct {
	TaskID           string             `json:"task_id,omitempty"`
	Attempt          int                `json:"attempt,omitempty"`
	Kind             string             `json:"kind,omitempty"`
	FilePath         string             `json:"file_path,omitempty"`
	ThumbnailPath    string             `json:"thumbnail_path,omitempty"`
	ChannelID        string             `json:"channel_id,omitempty"`
	ProfileDirectory string             `json:"profile_directory,omitempty"`
	Metadata         *contract.Metadata `json:"metadata,omitempty"`
	ExistingVideoID  string             `json:"existing_video_id,omitempty"`
}

// Task is one upload (or check) and everything known about it.
type Task struct {
	Spec      Spec   `json:"claim"`
	Token     string `json:"token"`
	Sum       string `json:"sha256"`
	Size      int64  `json:"size"`
	Ext       string `json:"ext"`
	VideoPath string `json:"video_path"`
	ThumbPath string `json:"thumb_path,omitempty"`
	VideoName string `json:"video_name"`
	ThumbName string `json:"thumb_name,omitempty"`
	// Stop makes every later task_* call answer control "stop".
	Stop bool `json:"stop"`
	// QueuedAt orders the profile's queue while Status is QUEUED.
	QueuedAt time.Time `json:"queued_at,omitempty"`
	// Holding is set from assignment until the profile is released
	// (session_ended, reject, or cancel without a launcher). While set, no
	// other task of the profile is assigned.
	Holding bool   `json:"holding,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	// ParentID is set on a check_video task: the upload task it checks.
	ParentID string `json:"parent_id,omitempty"`
	// VideoState is the last state read in Studio for this upload's video;
	// Rechecks counts the automatic checks while it was processing.
	VideoState *contract.VideoState `json:"video_state,omitempty"`
	Rechecks   int                  `json:"rechecks,omitempty"`

	Status    contract.Status    `json:"status"`
	Step      string             `json:"step,omitempty"`
	Progress  int                `json:"progress"`
	Message   string             `json:"message,omitempty"`
	VideoID   string             `json:"video_id,omitempty"`
	VideoURL  string             `json:"video_url,omitempty"`
	ErrorCode contract.ErrorCode `json:"error_code,omitempty"`
	Error     string             `json:"error,omitempty"`
	// Finish is nil until the agent calls task_finish for this attempt.
	Finish *FinishInfo `json:"finish,omitempty"`
	// Session is nil until the launcher reports that the harness exited.
	Session   *SessionInfo `json:"session,omitempty"`
	Events    []Event      `json:"events,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type FinishInfo struct {
	Status    string    `json:"status"`
	ErrorCode string    `json:"error_code,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	VideoID   string    `json:"video_id,omitempty"`
	VideoURL  string    `json:"-"`
	At        time.Time `json:"at"`
}

type SessionInfo struct {
	ExitCode   int       `json:"exit_code"`
	KilledBy   string    `json:"killed_by,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	EndedAt    time.Time `json:"ended_at"`
	// Runner is how the attempt ran: "playbook" (script only),
	// "playbook+llm" (the script handed steps to an LLM) or "llm".
	Runner string `json:"runner,omitempty"`
	// FailedSteps are the script steps that were handed to an LLM, in order.
	FailedSteps []string `json:"failed_steps,omitempty"`
	Handoffs    int      `json:"handoffs,omitempty"`
}

// Event is one line of a task's timeline: a task_mcp / report_mcp call by
// the agent (task_claim, task_report, task_video_created, task_finish) or
// something the server did (assigned, retry, cancel, rejected, session_ended).
type Event struct {
	Event    string    `json:"event"`
	Attempt  int       `json:"attempt"`
	Step     string    `json:"step,omitempty"`
	Progress int       `json:"progress,omitempty"`
	Status   string    `json:"status,omitempty"`
	VideoID  string    `json:"video_id,omitempty"`
	Message  string    `json:"message,omitempty"`
	At       time.Time `json:"at"`
}

// Errors of the task rules.
var (
	ErrNotRetryable   = errors.New("task cannot be retried")
	ErrSessionRunning = errors.New("the previous attempt's session is still running; retry after session_ended")
	ErrEnded          = errors.New("task already ended")
)

func (t *Task) ID() string      { return t.Spec.TaskID }
func (t *Task) Profile() string { return t.Spec.ProfileDirectory }
func (t *Task) Attempt() int    { return t.Spec.Attempt }

// Title is the video title of an upload, "" for other kinds.
func (t *Task) Title() string {
	if t.Spec.Metadata == nil {
		return ""
	}
	return t.Spec.Metadata.Title
}

// AddEvent appends a line to the timeline, stamped with the current attempt.
func (t *Task) AddEvent(e Event) {
	e.At = time.Now()
	e.Attempt = t.Spec.Attempt
	t.Events = append(t.Events, e)
	if n := len(t.Events); n > MaxEvents {
		t.Events = append([]Event(nil), t.Events[n-MaxEvents:]...)
	}
	t.UpdatedAt = e.At
}

// Claimed is the spec task_claim hands out. Once the video exists, a later
// claim (an LLM taking over from the launcher's script) must finish that
// video, not upload another.
func (t *Task) Claimed() Spec {
	s := t.Spec
	if s.ExistingVideoID == "" && t.VideoID != "" {
		s.ExistingVideoID = t.VideoID
	}
	return s
}

// Report records a task_report. The status follows the step only while the
// task is not stopped.
func (t *Task) Report(step, message string, progress int) {
	step = strings.ToUpper(strings.TrimSpace(step))
	t.AddEvent(Event{Event: "task_report", Step: step, Progress: progress, Message: message})
	if t.Stop {
		return
	}
	t.Step, t.Message = step, message
	if progress > 0 {
		t.Progress = progress
	}
	if contract.Status(step).In(contract.RunningStatuses) {
		t.Status = contract.Status(step)
	}
}

// VideoCreated records task_video_created.
func (t *Task) VideoCreated(id, url string) {
	t.AddEvent(Event{Event: "task_video_created", VideoID: id, Message: url})
	if id == "" {
		return
	}
	t.VideoID, t.VideoURL = id, url
	if t.VideoURL == "" {
		t.VideoURL = "https://youtu.be/" + id
	}
}

// Finished records task_finish and stops the attempt. A cancelled task stays
// cancelled; the finish is still recorded.
func (t *Task) Finished(f FinishInfo) {
	t.AddEvent(Event{Event: "task_finish", Status: f.Status, VideoID: f.VideoID, Message: f.Reason})
	f.At = time.Now()
	t.Finish = &f
	if f.VideoID != "" {
		t.VideoID = f.VideoID
		if f.VideoURL != "" {
			t.VideoURL = f.VideoURL
		} else if t.VideoURL == "" {
			t.VideoURL = "https://youtu.be/" + f.VideoID
		}
	}
	if t.Status != contract.StatusCancelled {
		switch strings.ToLower(f.Status) {
		case "done":
			t.Status, t.ErrorCode, t.Error = contract.StatusDone, "", ""
			t.Progress = 100
		case "needs_attention":
			t.Status, t.ErrorCode, t.Error = contract.StatusNeedsAttention, contract.ErrorCode(f.ErrorCode), f.Reason
		default:
			t.Status, t.ErrorCode, t.Error = contract.StatusFailed, contract.ErrorCode(f.ErrorCode), f.Reason
		}
	}
	t.Stop = true
}

// Rejected records that the launcher refused the attempt, and frees the
// profile.
func (t *Task) Rejected(reason string) {
	t.AddEvent(Event{Event: "rejected", Message: reason})
	if !t.Status.Terminal() {
		t.Status, t.Error, t.Stop = contract.StatusFailed, reason, true
	}
	t.Holding = false
}

// SessionEnded records that the harness exited. Without task_finish nobody
// will finish this attempt, so it is LOST. The profile is freed: Chrome and
// the tab are free only now. It returns the timeline message.
func (t *Task) SessionEnded(s SessionInfo) string {
	if s.EndedAt.IsZero() {
		s.EndedAt = time.Now()
	}
	t.Session = &s
	msg := "exit " + strconv.Itoa(s.ExitCode)
	if s.KilledBy != "" {
		msg += ", killed by " + s.KilledBy
	}
	t.AddEvent(Event{Event: "session_ended", Message: msg})
	if t.Finish == nil && !t.Status.Terminal() {
		t.Status, t.ErrorCode = contract.StatusLost, contract.ErrAgentLost
		t.Error = "session ended without task_finish"
		if s.Error != "" {
			t.Error += ": " + s.Error
		}
	}
	t.Stop = true
	t.Holding = false
	return msg
}

// SessionLost records that the launcher holding the task reconnected without
// its session (it restarted), and frees the profile.
func (t *Task) SessionLost(agentID string) {
	t.AddEvent(Event{Event: "session_lost", Message: "launcher " + agentID + " reconnected without this session"})
	if t.Finish == nil && !t.Status.Terminal() {
		t.Status, t.ErrorCode = contract.StatusLost, contract.ErrAgentLost
		t.Error = "launcher restarted; the session is gone"
	}
	t.Stop = true
	t.Holding = false
}

// Assigned records that the queue handed the attempt to a launcher.
func (t *Task) Assigned(agentID string) {
	t.Status, t.Holding, t.AgentID = contract.StatusAssigned, true, agentID
	t.AddEvent(Event{Event: "assigned", Message: agentID})
}

// NewAttempt prepares a retry: attempt+1, a new token (so a leftover session
// cannot report into it), existing_video_id when a video was created. The
// caller queues it.
func (t *Task) NewAttempt(token string) error {
	if !t.Status.In(contract.RetryableFrom) {
		return ErrNotRetryable
	}
	if t.Holding {
		return ErrSessionRunning
	}
	t.Token = token
	t.Spec.Attempt++
	if t.VideoID != "" {
		t.Spec.ExistingVideoID = t.VideoID
	}
	t.Stop, t.Finish, t.Session = false, nil, nil
	t.Step, t.Progress, t.Message, t.ErrorCode, t.Error = "", 0, "", "", ""
	t.AgentID = ""
	return nil
}

// Cancel stops the task. It reports whether it was still queued (then it
// just leaves the queue); a running one must also be told to its launcher.
func (t *Task) Cancel() (wasQueued bool, err error) {
	if t.Status.Terminal() {
		return false, ErrEnded
	}
	wasQueued = t.Status == contract.StatusQueued
	t.Stop = true
	t.Status = contract.StatusCancelled
	return wasQueued, nil
}

// SetVideoState stores what Studio showed and returns the timeline message.
func (t *Task) SetVideoState(vs contract.VideoState) string {
	if vs.CheckedAt.IsZero() {
		vs.CheckedAt = time.Now()
	}
	if vs.VideoID == "" {
		vs.VideoID = t.VideoID
	}
	t.VideoState = &vs
	msg := vs.Visibility
	if vs.Processing {
		msg += ", processing"
	}
	if vs.Restrictions != "" {
		msg += ", restrictions: " + vs.Restrictions
	}
	return msg
}

// Ended is true once an attempt of a check can no longer run on its own.
func (t *Task) Ended() bool {
	return t.Status.Terminal() || t.Status == contract.StatusLost || t.Status == contract.StatusNeedsAttention
}
