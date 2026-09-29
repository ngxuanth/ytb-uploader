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

type nopSender struct{}

func (nopSender) Send(string, any) error { return nil }

// fakeBrowsers records closes; block, when set, holds Close until closed.
type fakeBrowsers struct {
	mu     sync.Mutex
	closed []string
	block  chan struct{}
	inside chan struct{}
	once   sync.Once
}

func (b *fakeBrowsers) Close(_ context.Context, profile string) error {
	first := false
	b.once.Do(func() { first = true })
	if first && b.inside != nil {
		close(b.inside)
		<-b.block
	}
	b.mu.Lock()
	b.closed = append(b.closed, profile)
	b.mu.Unlock()
	return nil
}

func (b *fakeBrowsers) closedAt(i int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed[i]
}

func (b *fakeBrowsers) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.closed)
}

func assign(id string) contract.Assign {
	return contract.Assign{Task: contract.TaskSpec{TaskID: id, ProfileDirectory: "p"}, TaskMCP: contract.MCPEndpoint{URL: "x"}, ReportMCP: contract.MCPEndpoint{URL: "y"}}
}

// idleAgent runs every task instantly and reports each start on started.
func idleAgent(b *fakeBrowsers, started chan string) *Agent {
	a := &Agent{Timeout: time.Minute, Browsers: b, IdleClose: 50 * time.Millisecond,
		Exec: execFunc(func(_ context.Context, m contract.Assign, _ time.Duration) (upload.Summary, error) {
			started <- m.Task.TaskID
			return upload.Summary{}, nil
		})}
	a.Connected(nopSender{})
	return a
}

func waitIdle(t *testing.T, a *Agent) {
	t.Helper()
	for range 100 {
		if len(a.Heartbeat().Running) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("profile still held")
}

func TestChromeClosedAfterIdle(t *testing.T) {
	b := &fakeBrowsers{}
	started := make(chan string, 1)
	a := idleAgent(b, started)
	if r := a.Reserve(t.Context(), assign("t1")); r != "" {
		t.Fatal(r)
	}
	<-started
	waitIdle(t, a)
	time.Sleep(150 * time.Millisecond)
	if b.count() != 1 || b.closedAt(0) != "p" {
		t.Fatalf("closed %v", b.closed)
	}
}

func TestNextTaskKeepsChrome(t *testing.T) {
	b := &fakeBrowsers{}
	started := make(chan string, 2)
	a := idleAgent(b, started)
	a.Reserve(t.Context(), assign("t1"))
	<-started
	waitIdle(t, a)
	if r := a.Reserve(t.Context(), assign("t2")); r != "" {
		t.Fatal(r)
	}
	<-started
	waitIdle(t, a)
	time.Sleep(20 * time.Millisecond) // less than IdleClose after t2
	if b.count() != 0 {
		t.Fatalf("closed while in use: %v", b.closed)
	}
	time.Sleep(100 * time.Millisecond)
	if b.count() != 1 {
		t.Fatalf("closed %d times after t2", b.count())
	}
}

func TestTaskWaitsForClosingChrome(t *testing.T) {
	b := &fakeBrowsers{block: make(chan struct{}), inside: make(chan struct{})}
	started := make(chan string, 2)
	a := idleAgent(b, started)
	a.Reserve(t.Context(), assign("t1"))
	<-started
	waitIdle(t, a)
	<-b.inside // Close is running
	if r := a.Reserve(t.Context(), assign("t2")); r != "" {
		t.Fatal(r)
	}
	select {
	case id := <-started:
		t.Fatalf("%s started while Chrome was closing", id)
	case <-time.After(50 * time.Millisecond):
	}
	close(b.block)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("t2 never started")
	}
}
