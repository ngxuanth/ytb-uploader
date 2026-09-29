// Package taskservice holds the upload server's use cases: publishing an
// upload to its profile's queue, handing tasks to launchers, recording what
// the agent reports, retry and cancel, and video checks. Adapters (HTTP,
// MCP, WebSocket, the JSON store) call it; it talks to them only through the
// ports in ports.go.
package taskservice

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"strings"
	"sync"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// Config of the service.
type Config struct {
	// Base is the server's URL as launchers reach it (files, MCP).
	Base string
	// FinishGrace is how long a session may keep running after task_finish
	// before the server asks its launcher to stop it (0 = never).
	FinishGrace time.Duration
	// Recheck is how long after a "still processing" state the video is
	// checked again (0 = never).
	Recheck time.Duration
}

// DefaultConfig has the defaults of the server's flags.
func DefaultConfig(base string) Config {
	return Config{Base: base, FinishGrace: 45 * time.Second, Recheck: 0}
}

// maxRechecks bounds the automatic checks of a video still processing.
const maxRechecks = 6

type Service struct {
	cfg      Config
	repo     Repository
	profiles Profiles
	files    Files

	mu      sync.Mutex
	agents  map[AgentConn]*agent
	byID    map[string]*task.Task
	byToken map[string]*task.Task
}

// agent is one connected launcher and what it announced.
type agent struct {
	conn AgentConn
	info AgentInfo
}

func (a *agent) serves(profile string) bool {
	for _, p := range a.info.Profiles {
		if p.Directory == profile && p.Online {
			return true
		}
	}
	return false
}

// New loads the stored tasks.
func New(cfg Config, repo Repository, profiles Profiles, files Files) (*Service, error) {
	cfg.Base = strings.TrimRight(cfg.Base, "/")
	s := &Service{
		cfg: cfg, repo: repo, profiles: profiles, files: files,
		agents: map[AgentConn]*agent{}, byID: map[string]*task.Task{}, byToken: map[string]*task.Task{},
	}
	tasks, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		s.byID[t.ID()] = t
		s.byToken[t.Token] = t
	}
	return s, nil
}

// all is every task; the caller holds s.mu.
func (s *Service) all() []*task.Task {
	out := make([]*task.Task, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, t)
	}
	return out
}

// add registers a new task; the caller holds s.mu.
func (s *Service) add(t *task.Task) {
	s.byID[t.ID()] = t
	s.byToken[t.Token] = t
}

// saveLocked stores every task; a failure is logged (the next change saves
// again). The caller holds s.mu.
func (s *Service) saveLocked() {
	if err := s.repo.Save(s.all()); err != nil {
		log.Printf("save tasks: %v", err)
	}
}

// sendToLocked writes one message to the launcher with agentID.
func (s *Service) sendToLocked(agentID, msgType string, data any) error {
	for _, a := range s.agents {
		if a.info.AgentID == agentID {
			return a.conn.Send(msgType, data)
		}
	}
	return &Error{Kind: Conflict, Msg: "launcher " + agentID + " is not connected"}
}

// attemptLocked is the task if ref is its current attempt.
func (s *Service) attemptLocked(ref contract.TaskRef) *task.Task {
	t := s.byID[ref.TaskID]
	if t == nil || (ref.Attempt != 0 && ref.Attempt != t.Attempt()) {
		return nil
	}
	return t
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
