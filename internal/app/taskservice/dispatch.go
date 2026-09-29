package taskservice

import (
	"log"
	"sort"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// Launchers subscribe to the profiles they announce in hello; the queue of
// each profile hands its head to one of them when the profile is free.

// AgentHello registers a launcher that said hello on conn, settles the tasks
// it no longer runs, and hands out work.
func (s *Service) AgentHello(conn AgentConn, hello contract.Hello) {
	var dirs []string
	for _, p := range hello.Profiles {
		if p.Directory != "" {
			dirs = append(dirs, p.Directory)
		}
	}
	now := time.Now()
	s.mu.Lock()
	s.agents[conn] = &agent{conn: conn, info: AgentInfo{
		AgentID: hello.AgentID, Version: hello.Version, Profiles: hello.Profiles, ConnectedAt: now, LastSeen: now,
	}}
	s.reconcileLocked(hello)
	s.dispatchLocked()
	s.mu.Unlock()
	log.Printf("agent %s profiles: %s", hello.AgentID, strings.Join(dirs, ", "))
}

// AgentSeen notes a message from conn.
func (s *Service) AgentSeen(conn AgentConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.agents[conn]; a != nil {
		a.info.LastSeen = time.Now()
	}
}

// AgentGone unsubscribes conn. The tasks it holds keep their profile until
// it says hello again (reconcile) or they are cancelled.
func (s *Service) AgentGone(conn AgentConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, conn)
}

// Agents is every connected launcher, newest first.
func (s *Service) Agents() []AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AgentInfo{}
	for _, a := range s.agents {
		out = append(out, a.info)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ConnectedAt.After(out[b].ConnectedAt) })
	return out
}

// reconcileLocked runs on hello: a task the launcher held but no longer runs
// (it restarted) lost its session, so it is LOST and its profile freed.
func (s *Service) reconcileLocked(hello contract.Hello) {
	running := map[string]string{}
	for _, p := range hello.Profiles {
		running[p.Directory] = p.RunningTaskID
	}
	for _, t := range s.byID {
		if !t.Holding || t.AgentID != hello.AgentID {
			continue
		}
		if run, ok := running[t.Profile()]; ok && run == t.ID() {
			continue
		}
		t.SessionLost(hello.AgentID)
	}
}

// agentForLocked picks the most recently connected launcher serving profile.
func (s *Service) agentForLocked(profile string) *agent {
	var best *agent
	for _, a := range s.agents {
		if a.serves(profile) && (best == nil || a.info.ConnectedAt.After(best.info.ConnectedAt)) {
			best = a
		}
	}
	return best
}

// dispatchLocked assigns the head of every idle profile's queue to a
// launcher serving that profile, then saves.
func (s *Service) dispatchLocked() {
	all := s.all()
	for _, p := range task.Waiting(all) {
		if task.Holder(all, p) != nil {
			continue
		}
		a := s.agentForLocked(p)
		if a == nil {
			continue
		}
		t := task.Queue(all, p)[0]
		if err := a.conn.Send(contract.MsgAssign, s.assignment(t)); err != nil {
			// The socket is going away; the task stays first in line.
			log.Printf("assign %s to %s: %v", t.ID(), a.info.AgentID, err)
			continue
		}
		t.Assigned(a.info.AgentID)
		log.Printf("assigned %s attempt %d profile %s to %s", t.ID(), t.Attempt(), p, a.info.AgentID)
	}
	s.saveLocked()
}

// assignment is the assign message for t's current attempt.
func (s *Service) assignment(t *task.Task) contract.Assign {
	spec := contract.TaskSpec{
		TaskID: t.ID(), Attempt: t.Attempt(), ProfileDirectory: t.Profile(),
		SHA256: t.Sum, FileExt: t.Ext, Kind: t.Spec.Kind,
		ExistingVideoID: t.Spec.ExistingVideoID, FileSize: t.Size,
		// Big files get longer: the deadline grows with the upload.
		Deadline: time.Now().Add(max(45*time.Minute, contract.UploadBudget(t.Size))),
	}
	if t.VideoName != "" {
		spec.FileURL = s.cfg.Base + "/files/" + t.ID() + "/" + t.VideoName
	}
	if t.ThumbName != "" {
		spec.ThumbnailURL = s.cfg.Base + "/files/" + t.ID() + "/" + t.ThumbName
	}
	return contract.Assign{
		Task:      spec,
		TaskMCP:   contract.MCPEndpoint{URL: s.cfg.Base + "/task/mcp", Token: t.Token},
		ReportMCP: contract.MCPEndpoint{URL: s.cfg.Base + "/report/mcp", Token: t.Token},
	}
}

// Queues shows, per profile, the running task, the waiting ones in order,
// and whether a launcher serves it.
func (s *Service) Queues() []QueueView {
	names, _ := s.profiles.Names()
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.all()
	seen := map[string]bool{}
	for _, p := range names {
		seen[p] = true
	}
	for _, t := range all {
		if t.Holding || t.Status == contract.StatusQueued {
			seen[t.Profile()] = true
		}
	}
	out := []QueueView{}
	for p := range seen {
		v := QueueView{Profile: p, Queued: []string{}}
		if h := task.Holder(all, p); h != nil {
			v.Running = h.ID()
		}
		for _, t := range task.Queue(all, p) {
			v.Queued = append(v.Queued, t.ID())
		}
		if a := s.agentForLocked(p); a != nil {
			v.Subscribed, v.AgentID = true, a.info.AgentID
		}
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Profile < out[b].Profile })
	return out
}
