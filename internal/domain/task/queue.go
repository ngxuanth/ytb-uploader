package task

import (
	"sort"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// The queue rules. Tasks are published to a queue per Chrome profile and
// handed out one at a time: a profile's next task is assigned only after the
// running one is released (its harness exited, the launcher rejected it, or
// it was cancelled while no launcher held it).
//
// The queue is not stored separately: it is the profile's QUEUED tasks
// ordered by QueuedAt, and the running task is the one with Holding set.

// Holder is the task currently holding profile, or nil.
func Holder(all []*Task, profile string) *Task {
	for _, t := range all {
		if t.Holding && t.Profile() == profile {
			return t
		}
	}
	return nil
}

// Queue is profile's waiting tasks, first in line first.
func Queue(all []*Task, profile string) []*Task {
	var q []*Task
	for _, t := range all {
		if t.Status == contract.StatusQueued && t.Profile() == profile {
			q = append(q, t)
		}
	}
	sort.Slice(q, func(a, b int) bool { return q[a].QueuedAt.Before(q[b].QueuedAt) })
	return q
}

// Position is 1 for the next task in line, 0 when t is not queued.
func Position(all []*Task, t *Task) int {
	if t.Status != contract.StatusQueued {
		return 0
	}
	for i, q := range Queue(all, t.Profile()) {
		if q == t {
			return i + 1
		}
	}
	return 0
}

// Enqueue puts t in its profile's queue, at the end or (front) ahead of every
// waiting task.
func Enqueue(all []*Task, t *Task, front bool) {
	t.Status = contract.StatusQueued
	t.QueuedAt = time.Now()
	if !front {
		return
	}
	for _, q := range Queue(all, t.Profile()) {
		if q != t && !q.QueuedAt.After(t.QueuedAt) {
			t.QueuedAt = q.QueuedAt.Add(-time.Millisecond)
		}
	}
}

// Waiting lists the profiles that have a task in line.
func Waiting(all []*Task) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range all {
		if t.Status == contract.StatusQueued && !seen[t.Profile()] {
			seen[t.Profile()] = true
			out = append(out, t.Profile())
		}
	}
	sort.Strings(out)
	return out
}
