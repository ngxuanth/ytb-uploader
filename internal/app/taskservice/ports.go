package taskservice

import (
	"errors"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// Repository keeps every task. Save gets them all and replaces what was
// stored (the task set is small).
type Repository interface {
	Load() ([]*task.Task, error)
	Save([]*task.Task) error
}

// Profiles lists the Chrome profiles an upload may name.
type Profiles interface {
	Names() ([]string, error)
}

// FileInfo describes a local video or thumbnail.
type FileInfo struct {
	SHA256 string
	Size   int64
}

// Files reads the local files an upload points at.
type Files interface {
	Inspect(path string) (FileInfo, error)
	Exists(path string) error
}

// AgentConn is one connected launcher. Send is called with the service's
// lock held, so writes to one connection never interleave.
type AgentConn interface {
	Send(msgType string, data any) error
}

// ErrorKind says how an adapter should report an Error.
type ErrorKind int

const (
	Invalid ErrorKind = iota + 1
	NotFound
	Conflict
	Unauthorized
)

// Error is a use-case error an adapter maps to its protocol (HTTP status,
// MCP error). Known is set for an unknown profile: the valid ones.
type Error struct {
	Kind  ErrorKind
	Msg   string
	Known []string
}

func (e *Error) Error() string { return e.Msg }

func invalid(msg string) error  { return &Error{Kind: Invalid, Msg: msg} }
func notFound(msg string) error { return &Error{Kind: NotFound, Msg: msg} }
func conflict(msg string) error { return &Error{Kind: Conflict, Msg: msg} }

// ErrUnauthorized is returned for an unknown or ended session token.
var ErrUnauthorized = &Error{Kind: Unauthorized, Msg: "unauthorized"}

// KindOf is the ErrorKind of err, 0 for other errors.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}
