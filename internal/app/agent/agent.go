// Package agent is the launcher's side of the server protocol: it takes
// assigns (one running task per Chrome profile), cancels and drain, and
// tells the server how each attempt ended. The connection and the attempt
// itself are behind the Sender and Executor ports.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Sender is the live connection to the server.
type Sender interface {
	Send(msgType string, data any) error
}

// Profile is a Chrome profile on this machine.
type Profile struct {
	Directory string
	Email     string
}

// Executor runs one attempt. An error means it could not start (download,
// Chrome); the server is told the attempt was rejected.
type Executor interface {
	Execute(ctx context.Context, msg contract.Assign, timeout time.Duration) (upload.Summary, error)
}

// Browsers closes a profile's Chrome.
type Browsers interface {
	Close(ctx context.Context, profile string) error
}

// Agent is the launcher's state.
type Agent struct {
	ID       string
	Version  string
	Timeout  time.Duration // session timeout; a big file gets more
	Profiles func() ([]Profile, error)
	Exec     Executor
	Logf     func(string, ...any)
	// Browsers and IdleClose close a profile's Chrome once no task has used
	// it for IdleClose after the last one ended (0 or nil: never).
	Browsers  Browsers
	IdleClose time.Duration

	mu      sync.Mutex
	conn    Sender
	drain   bool
	running map[string]*runningTask  // by profile
	idle    map[string]*time.Timer   // profile -> pending close
	closing map[string]chan struct{} // profile -> closed when its Chrome is down
}

type runningTask struct {
	taskID  string
	attempt int
	cancel  context.CancelFunc
}

func (a *Agent) logf(f string, args ...any) {
	if a.Logf != nil {
		a.Logf(f, args...)
	}
}

// Connected makes s the connection messages go to.
func (a *Agent) Connected(s Sender) {
	a.mu.Lock()
	a.conn = s
	a.mu.Unlock()
}

// Disconnected forgets s if it is still the current connection.
func (a *Agent) Disconnected(s Sender) {
	a.mu.Lock()
	if a.conn == s {
		a.conn = nil
	}
	a.mu.Unlock()
}

// Hello announces the profiles and what runs on them.
func (a *Agent) Hello() contract.Hello {
	var ps []Profile
	if a.Profiles != nil {
		var err error
		if ps, err = a.Profiles(); err != nil {
			a.logf("profiles: %v", err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var profiles []contract.ProfileState
	for _, p := range ps {
		st := contract.ProfileState{Directory: p.Directory, Email: p.Email, Online: true}
		if run, ok := a.running[p.Directory]; ok {
			st.RunningTaskID = run.taskID
		}
		profiles = append(profiles, st)
	}
	return contract.Hello{AgentID: a.ID, Version: a.Version, Profiles: profiles}
}

// Heartbeat lists the running tasks.
func (a *Agent) Heartbeat() contract.Heartbeat {
	a.mu.Lock()
	defer a.mu.Unlock()
	var running []contract.RunningTask
	for _, r := range a.running {
		running = append(running, contract.RunningTask{TaskID: r.taskID, Attempt: r.attempt, Status: contract.StatusAssigned})
	}
	return contract.Heartbeat{Running: running}
}

// Handle acts on one message from the server.
func (a *Agent) Handle(ctx context.Context, env contract.Envelope) {
	switch env.Type {
	case contract.MsgAssign:
		var msg contract.Assign
		if err := unmarshal(env, &msg); err != nil {
			a.logf("assign: %v", err)
			return
		}
		if reason := a.Reserve(ctx, msg); reason != "" {
			a.logf("reject %s: %s", msg.Task.TaskID, reason)
			a.reject(msg.Task, reason)
		}
	case contract.MsgCancel:
		var msg contract.Cancel
		if err := unmarshal(env, &msg); err != nil {
			a.logf("cancel: %v", err)
			return
		}
		a.Cancel(msg.TaskID)
	case contract.MsgDrain:
		var msg contract.Drain
		if err := unmarshal(env, &msg); err != nil {
			a.logf("drain: %v", err)
			return
		}
		a.mu.Lock()
		a.drain = msg.Enabled
		a.mu.Unlock()
		a.logf("drain=%v", msg.Enabled)
	}
}

// Reserve starts the task or returns why it was refused.
func (a *Agent) Reserve(ctx context.Context, msg contract.Assign) string {
	task := msg.Task
	if task.ProfileDirectory == "" || task.TaskID == "" {
		return "task is missing profile_directory or task_id"
	}
	if msg.TaskMCP.URL == "" || msg.ReportMCP.URL == "" {
		return "task is missing task_mcp or report_mcp url"
	}
	a.mu.Lock()
	if a.drain {
		a.mu.Unlock()
		return "agent is draining"
	}
	if a.running == nil {
		a.running = map[string]*runningTask{}
	}
	if _, ok := a.running[task.ProfileDirectory]; ok {
		a.mu.Unlock()
		return "profile is busy"
	}
	sessCtx, cancel := context.WithCancel(ctx)
	a.running[task.ProfileDirectory] = &runningTask{taskID: task.TaskID, attempt: task.Attempt, cancel: cancel}
	// The profile is in use again: keep its Chrome.
	if t, ok := a.idle[task.ProfileDirectory]; ok {
		t.Stop()
		delete(a.idle, task.ProfileDirectory)
	}
	closing := a.closing[task.ProfileDirectory]
	a.mu.Unlock()

	go func() {
		if closing != nil {
			// Its Chrome is being closed; open it again only once it is down.
			select {
			case <-closing:
			case <-sessCtx.Done():
			}
		}
		a.execute(sessCtx, cancel, msg)
	}()
	return ""
}

func (a *Agent) execute(ctx context.Context, cancel context.CancelFunc, msg contract.Assign) {
	defer cancel()
	defer a.release(msg.Task.ProfileDirectory, msg.Task.TaskID)
	task := msg.Task
	// A big file needs longer than the default session timeout.
	timeout := max(a.Timeout, contract.UploadBudget(task.FileSize))
	if !task.Deadline.IsZero() {
		if d := time.Until(task.Deadline); d < timeout {
			timeout = d
		}
	}
	if timeout <= 0 {
		a.logf("task %s deadline already passed", task.TaskID)
		return
	}
	began := time.Now()
	sum, err := a.Exec.Execute(ctx, msg, timeout)
	if err != nil {
		a.logf("task %s: %v", task.TaskID, err)
		a.reject(task, err.Error())
		return
	}
	sum.Out.Duration = time.Since(began)
	a.sessionEnded(task, sum)
	if sum.Err != nil {
		a.logf("task %s: %v", task.TaskID, sum.Err)
		return
	}
	a.logf("task %s ended: runner=%s failed_steps=%v exit=%d killed_by=%q duration=%s", task.TaskID, sum.Runner, sum.FailedSteps, sum.Out.ExitCode, sum.Out.KilledBy, sum.Out.Duration.Round(time.Second))
}

// sessionEnded tells the server the attempt ended, so it can tell a session
// that called task_finish from one that stopped without it.
func (a *Agent) sessionEnded(task contract.TaskSpec, sum upload.Summary) {
	out := sum.Out
	data := map[string]any{
		"exit_code": out.ExitCode, "killed_by": out.KilledBy, "duration_ms": out.Duration.Milliseconds(),
		"runner": sum.Runner, "failed_steps": sum.FailedSteps, "handoffs": sum.Handoffs,
	}
	if sum.Err != nil {
		data["error"] = sum.Err.Error()
	}
	_ = a.send(contract.MsgEvent, contract.Event{
		TaskRef: contract.TaskRef{TaskID: task.TaskID, Attempt: task.Attempt},
		Type:    contract.EventSessionEnded, Data: data, At: time.Now(),
	})
}

func (a *Agent) reject(task contract.TaskSpec, reason string) {
	_ = a.send(contract.MsgReject, contract.Reject{
		TaskRef: contract.TaskRef{TaskID: task.TaskID, Attempt: task.Attempt},
		Reason:  reason,
	})
}

func (a *Agent) send(typ string, data any) error {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	return conn.Send(typ, data)
}

func (a *Agent) release(profile, taskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if run, ok := a.running[profile]; !ok || run.taskID != taskID {
		return
	}
	delete(a.running, profile)
	if a.Browsers == nil || a.IdleClose <= 0 {
		return
	}
	if a.idle == nil {
		a.idle = map[string]*time.Timer{}
	}
	if t, ok := a.idle[profile]; ok {
		t.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(a.IdleClose, func() { a.closeIdle(profile, t) })
	a.idle[profile] = t
}

// closeIdle closes profile's Chrome if timer t is still its pending close
// and no task took the profile meanwhile.
func (a *Agent) closeIdle(profile string, t *time.Timer) {
	a.mu.Lock()
	if a.idle[profile] != t {
		a.mu.Unlock()
		return
	}
	delete(a.idle, profile)
	if _, busy := a.running[profile]; busy {
		a.mu.Unlock()
		return
	}
	if a.closing == nil {
		a.closing = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	a.closing[profile] = done
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := a.Browsers.Close(ctx, profile)
	cancel()
	if err != nil {
		a.logf("profile %s idle %s; close Chrome: %v", profile, a.IdleClose, err)
	} else {
		a.logf("profile %s idle %s; closed Chrome", profile, a.IdleClose)
	}
	a.mu.Lock()
	delete(a.closing, profile)
	a.mu.Unlock()
	close(done)
}

// Cancel stops the task if it runs here.
func (a *Agent) Cancel(taskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, run := range a.running {
		if run.taskID == taskID {
			a.logf("cancel %s", taskID)
			run.cancel()
			return
		}
	}
}

// StopAll cancels every running task.
func (a *Agent) StopAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, run := range a.running {
		run.cancel()
	}
}

func unmarshal(env contract.Envelope, dest any) error {
	if len(env.Data) == 0 {
		return errors.New("empty data")
	}
	return json.Unmarshal(env.Data, dest)
}
