package main

import (
	"log"
	"sort"
	"time"

	"github.com/gofiber/contrib/v3/websocket"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// Tasks are published to a queue per Chrome profile and handed out one at a
// time: a profile's next task is assigned only after the running one is
// released, i.e. its harness exited (session_ended), the launcher rejected
// it, or it was cancelled while no launcher held it. Launchers subscribe to
// the profiles they announce in hello.
//
// The queue is not stored separately: it is the profile's QUEUED jobs ordered
// by QueuedAt, and the running task is the job with Holding set.

// agentConn is one connected launcher.
type agentConn struct {
	conn *websocket.Conn
	info agentInfo
}

func (a *agentConn) serves(profile string) bool {
	for _, p := range a.info.Profiles {
		if p.Directory == profile && p.Online {
			return true
		}
	}
	return false
}

// holderLocked returns the job currently holding profile, if any.
func (st *state) holderLocked(profile string) *job {
	for _, j := range st.byID {
		if j.Holding && j.Claim.ProfileDirectory == profile {
			return j
		}
	}
	return nil
}

// queueLocked returns profile's waiting jobs, first in line first.
func (st *state) queueLocked(profile string) []*job {
	var q []*job
	for _, j := range st.byID {
		if j.Status == wire.StatusQueued && j.Claim.ProfileDirectory == profile {
			q = append(q, j)
		}
	}
	sort.Slice(q, func(a, b int) bool { return q[a].QueuedAt.Before(q[b].QueuedAt) })
	return q
}

// queuePositionLocked is 1 for the next job in line, 0 if j is not queued.
func (st *state) queuePositionLocked(j *job) int {
	if j.Status != wire.StatusQueued {
		return 0
	}
	for i, q := range st.queueLocked(j.Claim.ProfileDirectory) {
		if q == j {
			return i + 1
		}
	}
	return 0
}

// agentForLocked picks the most recently connected launcher serving profile.
func (st *state) agentForLocked(profile string) *agentConn {
	var best *agentConn
	for _, a := range st.agents {
		if a.serves(profile) && (best == nil || a.info.ConnectedAt.After(best.info.ConnectedAt)) {
			best = a
		}
	}
	return best
}

// enqueueLocked puts j in its profile's queue, at the end or (front) ahead of
// every waiting job.
func (st *state) enqueueLocked(j *job, front bool) {
	j.Status = wire.StatusQueued
	j.QueuedAt = time.Now()
	if front {
		for _, q := range st.queueLocked(j.Claim.ProfileDirectory) {
			if q != j && !q.QueuedAt.After(j.QueuedAt) {
				j.QueuedAt = q.QueuedAt.Add(-time.Millisecond)
			}
		}
	}
}

// dispatchLocked assigns the head of every idle profile's queue to a
// launcher serving that profile, then saves. The caller holds st.mu.
func (st *state) dispatchLocked() {
	profiles := map[string]bool{}
	for _, j := range st.byID {
		if j.Status == wire.StatusQueued {
			profiles[j.Claim.ProfileDirectory] = true
		}
	}
	for p := range profiles {
		if st.holderLocked(p) != nil {
			continue
		}
		a := st.agentForLocked(p)
		if a == nil {
			continue
		}
		j := st.queueLocked(p)[0]
		msg, err := wire.NewEnvelope(wire.MsgAssign, st.assign(j))
		if err == nil {
			err = a.conn.WriteJSON(msg)
		}
		if err != nil {
			// The socket is going away; the job stays first in line.
			log.Printf("assign %s to %s: %v", j.Claim.TaskID, a.info.AgentID, err)
			continue
		}
		j.Status, j.Holding, j.AgentID = wire.StatusAssigned, true, a.info.AgentID
		j.addEvent(eventEntry{Event: "assigned", Message: a.info.AgentID})
		log.Printf("assigned %s attempt %d profile %s to %s", j.Claim.TaskID, j.Claim.Attempt, p, a.info.AgentID)
	}
	st.saveLocked()
}

// releaseLocked frees j's profile so the next queued task can start. The
// caller dispatches afterwards.
func (st *state) releaseLocked(j *job) {
	j.Holding = false
}

// reconcileLocked runs on hello: a launcher that no longer runs a task it was
// holding (it restarted) lost that session, so the task is LOST and the
// profile freed.
func (st *state) reconcileLocked(hello wire.Hello) {
	running := map[string]string{}
	for _, p := range hello.Profiles {
		running[p.Directory] = p.RunningTaskID
	}
	for _, j := range st.byID {
		if !j.Holding || j.AgentID != hello.AgentID {
			continue
		}
		if run, ok := running[j.Claim.ProfileDirectory]; ok && run == j.Claim.TaskID {
			continue
		}
		j.addEvent(eventEntry{Event: "session_lost", Message: "launcher " + hello.AgentID + " reconnected without this session"})
		if j.Finish == nil && !j.Status.Terminal() {
			j.Status, j.ErrorCode = wire.StatusLost, wire.ErrAgentLost
			j.Error = "launcher restarted; the session is gone"
		}
		j.Stop = true
		st.releaseLocked(j)
	}
}

// queueView is one profile in GET /queues.
type queueView struct {
	Profile    string   `json:"profile"`
	Running    string   `json:"running,omitempty"`
	Queued     []string `json:"queued"`
	Subscribed bool     `json:"subscribed"`
	AgentID    string   `json:"agent_id,omitempty"`
}

func (st *state) queuesLocked(profiles []string) []queueView {
	seen := map[string]bool{}
	for _, p := range profiles {
		seen[p] = true
	}
	for _, j := range st.byID {
		if j.Holding || j.Status == wire.StatusQueued {
			seen[j.Claim.ProfileDirectory] = true
		}
	}
	out := []queueView{}
	for p := range seen {
		v := queueView{Profile: p, Queued: []string{}}
		if h := st.holderLocked(p); h != nil {
			v.Running = h.Claim.TaskID
		}
		for _, j := range st.queueLocked(p) {
			v.Queued = append(v.Queued, j.Claim.TaskID)
		}
		if a := st.agentForLocked(p); a != nil {
			v.Subscribed, v.AgentID = true, a.info.AgentID
		}
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Profile < out[b].Profile })
	return out
}
