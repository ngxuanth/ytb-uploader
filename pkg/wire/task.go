// Package wire holds the types shared by the server and the agent: task
// states, error codes and the WebSocket messages between them.
package wire

import "time"

type Status string

const (
	StatusQueued          Status = "QUEUED"
	StatusAssigned        Status = "ASSIGNED"
	StatusDownloading     Status = "DOWNLOADING"
	StatusPreparing       Status = "PREPARING"
	StatusAttaching       Status = "ATTACHING"
	StatusFillingMetadata Status = "FILLING_METADATA"
	StatusUploading       Status = "UPLOADING"
	StatusProcessing      Status = "PROCESSING"
	StatusPublishing      Status = "PUBLISHING"
	StatusCancelling      Status = "CANCELLING"

	StatusDone           Status = "DONE"
	StatusFailed         Status = "FAILED"
	StatusNeedsAttention Status = "NEEDS_ATTENTION"
	StatusCancelled      Status = "CANCELLED"
	StatusLost           Status = "LOST"
)

// ActiveStatuses are the states in which a task holds its profile. At most
// one task per profile may be in one of them (enforced by a unique index).
var ActiveStatuses = []Status{
	StatusAssigned, StatusDownloading, StatusPreparing, StatusAttaching,
	StatusFillingMetadata, StatusUploading, StatusProcessing, StatusPublishing,
	StatusCancelling,
}

// RunningStatuses are the progress states an agent may report.
var RunningStatuses = []Status{
	StatusDownloading, StatusPreparing, StatusAttaching, StatusFillingMetadata,
	StatusUploading, StatusProcessing, StatusPublishing,
}

// RetryableFrom are the states a manual retry may start from.
var RetryableFrom = []Status{StatusFailed, StatusCancelled, StatusLost, StatusNeedsAttention}

func (s Status) In(set []Status) bool {
	for _, x := range set {
		if s == x {
			return true
		}
	}
	return false
}

func (s Status) Active() bool   { return s.In(ActiveStatuses) }
func (s Status) Terminal() bool { return s.In([]Status{StatusDone, StatusFailed, StatusCancelled}) }

type ErrorCode string

const (
	ErrLoginRequired      ErrorCode = "LOGIN_REQUIRED"
	ErrWrongChannel       ErrorCode = "WRONG_CHANNEL"
	ErrUploadLimit        ErrorCode = "UPLOAD_LIMIT"
	ErrPlaylistNotFound   ErrorCode = "PLAYLIST_NOT_FOUND"
	ErrUnsupported        ErrorCode = "UNSUPPORTED"
	ErrStepFailed         ErrorCode = "STEP_FAILED"
	ErrTimeout            ErrorCode = "TIMEOUT"
	ErrBrowserOffline     ErrorCode = "BROWSER_OFFLINE"
	ErrDownloadFailed     ErrorCode = "DOWNLOAD_FAILED"
	ErrPublishUnconfirmed ErrorCode = "PUBLISH_UNCONFIRMED"
	ErrAgentLost          ErrorCode = "AGENT_LOST"
	ErrFileExpired        ErrorCode = "FILE_EXPIRED"
	ErrInternal           ErrorCode = "INTERNAL"
)

// Retryable errors are retried automatically (same profile, with backoff)
// while attempts remain.
func (c ErrorCode) Retryable() bool {
	switch c {
	case ErrStepFailed, ErrTimeout, ErrBrowserOffline, ErrDownloadFailed, ErrInternal:
		return true
	}
	return false
}

// NeedsAttention errors need a person (log in, pick the right channel, wait
// out a limit) before a retry can succeed.
func (c ErrorCode) NeedsAttention() bool {
	switch c {
	case ErrLoginRequired, ErrWrongChannel, ErrUploadLimit:
		return true
	}
	return false
}

type Visibility string

const (
	VisibilityPublic   Visibility = "public"
	VisibilityUnlisted Visibility = "unlisted"
	VisibilityPrivate  Visibility = "private"
)

// Metadata is what gets filled into the YouTube Studio upload dialog.
type Metadata struct {
	Title       string     `json:"title" validate:"required,max=100"`
	Description string     `json:"description,omitempty" validate:"max=5000"`
	Tags        []string   `json:"tags,omitempty"`
	Playlists   []string   `json:"playlists,omitempty"`
	Visibility  Visibility `json:"visibility" validate:"required,oneof=public unlisted private"`
	MadeForKids bool       `json:"made_for_kids"`
	ScheduleAt  *time.Time `json:"schedule_at,omitempty"`
}

// UploadBudget is how long an upload of a size-byte file may take end to
// end: a fixed part for opening Studio, filling the details and YouTube's
// checks, plus sending the file at a slow 512 KiB/s. A 2 MB clip gets about
// 20 minutes, 1 GB about 54 minutes. The server uses it for the task
// deadline, the launcher for the session, the playbook for its upload wait.
func UploadBudget(size int64) time.Duration {
	const base = 20 * time.Minute
	const rate = 512 << 10 // bytes per second
	if size < 0 {
		size = 0
	}
	return base + time.Duration(size/rate)*time.Second
}
