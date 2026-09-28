package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// job is one upload task and everything known about it. It is saved as JSON,
// so a restarted server keeps its tasks and running sessions keep their tokens.
type job struct {
	Claim     *taskmcp.ClaimOut `json:"claim"`
	Token     string            `json:"token"`
	Sum       string            `json:"sha256"`
	Ext       string            `json:"ext"`
	VideoPath string            `json:"video_path"`
	ThumbPath string            `json:"thumb_path,omitempty"`
	VideoName string            `json:"video_name"`
	ThumbName string            `json:"thumb_name,omitempty"`
	// Stop makes every later task_* call answer control "stop".
	Stop bool `json:"stop"`

	Status    wire.Status    `json:"status"`
	Step      string         `json:"step,omitempty"`
	Progress  int            `json:"progress"`
	Message   string         `json:"message,omitempty"`
	VideoID   string         `json:"video_id,omitempty"`
	VideoURL  string         `json:"video_url,omitempty"`
	ErrorCode wire.ErrorCode `json:"error_code,omitempty"`
	Error     string         `json:"error,omitempty"`
	// Finish is nil until the agent calls task_finish for this attempt.
	Finish *finishInfo `json:"finish,omitempty"`
	// Session is nil until the launcher reports that the harness exited.
	Session   *sessionInfo `json:"session,omitempty"`
	Events    []eventEntry `json:"events,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type finishInfo struct {
	Status    string    `json:"status"`
	ErrorCode string    `json:"error_code,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	VideoID   string    `json:"video_id,omitempty"`
	At        time.Time `json:"at"`
}

type sessionInfo struct {
	ExitCode   int       `json:"exit_code"`
	KilledBy   string    `json:"killed_by,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	EndedAt    time.Time `json:"ended_at"`
}

// eventEntry is one line of a task's timeline: a task_mcp / report_mcp call
// by the agent (task_claim, task_report, task_video_created, task_finish) or
// something the server did (assigned, retry, cancel, rejected, session_ended).
type eventEntry struct {
	Event    string    `json:"event"`
	Attempt  int       `json:"attempt"`
	Step     string    `json:"step,omitempty"`
	Progress int       `json:"progress,omitempty"`
	Status   string    `json:"status,omitempty"`
	VideoID  string    `json:"video_id,omitempty"`
	Message  string    `json:"message,omitempty"`
	At       time.Time `json:"at"`
}

// maxEvents caps a task's timeline; the oldest lines are dropped first.
const maxEvents = 200

func (j *job) addEvent(e eventEntry) {
	e.At = time.Now()
	e.Attempt = j.Claim.Attempt
	j.Events = append(j.Events, e)
	if n := len(j.Events); n > maxEvents {
		j.Events = append([]eventEntry(nil), j.Events[n-maxEvents:]...)
	}
	j.UpdatedAt = e.At
}

// store keeps every job in one JSON file.
type store struct{ path string }

func (s *store) load() ([]*job, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []*job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// save writes a temporary file and renames it over the old one, so a crash
// never leaves a half-written file.
func (s *store) save(jobs []*job) error {
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].CreatedAt.Before(jobs[b].CreatedAt) })
	raw, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
