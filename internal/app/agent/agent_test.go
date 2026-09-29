package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

func TestReserveRejectsBusyAndDrain(t *testing.T) {
	a := &Agent{running: map[string]*runningTask{}}
	msg := contract.Assign{Task: contract.TaskSpec{TaskID: "t1", ProfileDirectory: "Default"}, TaskMCP: contract.MCPEndpoint{URL: "http://task"}, ReportMCP: contract.MCPEndpoint{URL: "http://report"}}
	a.drain = true
	if reason := a.Reserve(t.Context(), msg); reason != "agent is draining" {
		t.Fatalf("drain: %q", reason)
	}
	a.drain = false
	a.running["Default"] = &runningTask{taskID: "other"}
	if reason := a.Reserve(t.Context(), msg); reason != "profile is busy" {
		t.Fatalf("busy: %q", reason)
	}
	if reason := a.Reserve(t.Context(), contract.Assign{}); reason == "" {
		t.Fatal("empty assign should be rejected")
	}
}

type sent struct {
	mu   sync.Mutex
	msgs []string
	done chan struct{}
}

func (s *sent) Send(typ string, _ any) error {
	s.mu.Lock()
	s.msgs = append(s.msgs, typ)
	s.mu.Unlock()
	close(s.done)
	return nil
}

type execFunc func(context.Context, contract.Assign, time.Duration) (upload.Summary, error)

func (f execFunc) Execute(ctx context.Context, m contract.Assign, d time.Duration) (upload.Summary, error) {
	return f(ctx, m, d)
}

func TestExecuteReportsSessionEndedAndFreesProfile(t *testing.T) {
	s := &sent{done: make(chan struct{})}
	a := &Agent{Timeout: time.Minute, Exec: execFunc(func(context.Context, contract.Assign, time.Duration) (upload.Summary, error) {
		return upload.Summary{Runner: upload.RunnerPlaybook}, nil
	})}
	a.Connected(s)
	msg := contract.Assign{Task: contract.TaskSpec{TaskID: "t1", ProfileDirectory: "p"}, TaskMCP: contract.MCPEndpoint{URL: "x"}, ReportMCP: contract.MCPEndpoint{URL: "y"}}
	if reason := a.Reserve(t.Context(), msg); reason != "" {
		t.Fatal(reason)
	}
	<-s.done
	if s.msgs[0] != contract.MsgEvent {
		t.Fatalf("sent %v", s.msgs)
	}
	for range 50 {
		if len(a.Heartbeat().Running) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("profile still held")
}
