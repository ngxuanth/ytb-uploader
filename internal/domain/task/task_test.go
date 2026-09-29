package task

import (
	"testing"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

func newTask(id, profile string) *Task {
	return &Task{Spec: Spec{TaskID: id, ProfileDirectory: profile, Attempt: 1}}
}

func TestQueueOrderAndFront(t *testing.T) {
	a, b, c := newTask("a", "p"), newTask("b", "p"), newTask("c", "q")
	all := []*Task{a, b, c}
	Enqueue(all, a, false)
	time.Sleep(2 * time.Millisecond)
	Enqueue(all, b, false)
	Enqueue(all, c, false)
	if Position(all, a) != 1 || Position(all, b) != 2 || Position(all, c) != 1 {
		t.Fatalf("positions %d %d %d", Position(all, a), Position(all, b), Position(all, c))
	}
	Enqueue(all, b, true)
	if q := Queue(all, "p"); q[0] != b || q[1] != a {
		t.Fatalf("front: %s %s", q[0].ID(), q[1].ID())
	}
	a.Assigned("agent")
	if Holder(all, "p") != a || Position(all, a) != 0 {
		t.Fatal("holder")
	}
	if w := Waiting(all); len(w) != 2 || w[0] != "p" || w[1] != "q" {
		t.Fatalf("waiting %v", w)
	}
}

func TestSessionEndedWithoutFinishIsLost(t *testing.T) {
	x := newTask("x", "p")
	x.Assigned("agent")
	x.Report("uploading", "", 10)
	if x.Status != contract.StatusUploading {
		t.Fatalf("status %s", x.Status)
	}
	x.SessionEnded(SessionInfo{ExitCode: 1})
	if x.Status != contract.StatusLost || x.ErrorCode != contract.ErrAgentLost || x.Holding || !x.Stop {
		t.Fatalf("after end: %+v", x)
	}
}

func TestFinishThenRetryKeepsVideo(t *testing.T) {
	x := newTask("x", "p")
	x.Assigned("agent")
	x.VideoCreated("vid", "")
	x.Finished(FinishInfo{Status: "failed", Reason: "boom"})
	if x.Status != contract.StatusFailed || x.VideoURL != "https://youtu.be/vid" {
		t.Fatalf("finish: %+v", x)
	}
	if err := x.NewAttempt("tok"); err != ErrSessionRunning {
		t.Fatalf("retry while holding: %v", err)
	}
	x.SessionEnded(SessionInfo{})
	if x.Status != contract.StatusFailed {
		t.Fatalf("finished task must not become LOST: %s", x.Status)
	}
	if err := x.NewAttempt("tok"); err != nil {
		t.Fatal(err)
	}
	if x.Attempt() != 2 || x.Spec.ExistingVideoID != "vid" || x.Stop || x.Finish != nil {
		t.Fatalf("new attempt: %+v", x)
	}
	Enqueue([]*Task{x}, x, false)
	if queued, err := x.Cancel(); err != nil || !queued || x.Status != contract.StatusCancelled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := x.Cancel(); err != ErrEnded {
		t.Fatalf("cancel twice: %v", err)
	}
}
