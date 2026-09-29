package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fasthttp/websocket"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/chromectl"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/harness"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/session"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

const agentVersion = "0.1.0"

// runAgent stays connected to the server. Each assign starts one harness on
// that profile. task_mcp and report_mcp stay on the server.
func runAgent(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	serverURL := fs.String("server", "", "agent WebSocket URL (required)")
	agentID := fs.String("id", "", "agent id announced in hello (default: hostname)")
	preset := fs.String("harness", "hermes", "harness preset: claude, cursor, codex, hermes, custom")
	bin := fs.String("bin", "", "override the CLI binary")
	model := fs.String("model", "", "override the model")
	bmcp := fs.String("bmcp", defaultBMCP(), "path to bmcp dist/index.js")
	timeout := fs.Duration("timeout", 45*time.Minute, "kill a session after this long")
	workDir := fs.String("work", "./data/run", "where session files and transcripts go")
	userDataDir := fs.String("user-data-dir", defaultUserDataDir(), "Chrome user-data-dir holding the profiles")
	debugPort := fs.Int("debug-port", 9222, "Chrome remote debugging port")
	extDir := fs.String("extension", defaultExtensionDir(), "unpacked Browser MCP extension directory")
	chromeBin := fs.String("chrome-bin", "google-chrome", "Chrome binary")
	runner := fs.String("runner", runnerPlaybook, "playbook: script first, LLM only for failed steps; llm: an LLM session does every task")
	failAt := fs.String("playbook-fail-at", "", "testing: make this playbook step fail once to exercise the LLM hand-over")
	_ = fs.Parse(args)
	if *serverURL == "" {
		fs.Usage()
		return errors.New("-server is required")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	absUDD, err := filepath.Abs(*userDataDir)
	if err != nil {
		return err
	}
	absExt, err := filepath.Abs(*extDir)
	if err != nil {
		return err
	}
	absWork, err := filepath.Abs(*workDir)
	if err != nil {
		return err
	}
	spec, err := harness.Resolve(harness.Spec{Preset: *preset, Bin: *bin, Model: *model})
	if err != nil {
		return err
	}
	id := *agentID
	if id == "" {
		id, _ = os.Hostname()
	}
	a := &agent{
		url: *serverURL, id: id, spec: spec, bmcp: *bmcp, launcher: self,
		work: absWork, timeout: *timeout, runner: *runner, failAt: *failAt,
		chrome:  &chromectl.Controller{Bin: *chromeBin, UserDataDir: absUDD, DebugPort: *debugPort, ExtensionDir: absExt},
		running: map[string]*runningTask{},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return a.loop(ctx)
}

type runningTask struct {
	taskID  string
	attempt int
	cancel  context.CancelFunc
}

type agent struct {
	url, id, bmcp, launcher, work string
	spec                          harness.Spec
	timeout                       time.Duration
	runner, failAt                string
	chrome                        *chromectl.Controller

	mu      sync.Mutex
	conn    *websocket.Conn
	drain   bool
	running map[string]*runningTask
}

func (a *agent) loop(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			a.stopAll()
			return nil
		}
		err := a.serve(ctx)
		if ctx.Err() != nil {
			a.stopAll()
			return nil
		}
		logf("socket closed: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			a.stopAll()
			return nil
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (a *agent) serve(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		a.mu.Unlock()
	}()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// ReadJSON below does not watch ctx; closing the socket on shutdown is
	// what unblocks it, so Ctrl+C really stops the launcher.
	go func() {
		<-connCtx.Done()
		conn.Close()
	}()
	go a.heartbeat(connCtx, conn)
	if err := a.send(conn, wire.MsgHello, a.hello()); err != nil {
		return err
	}
	logf("connected to %s", a.url)
	for {
		var env wire.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}
		a.onMessage(ctx, conn, env)
	}
}

func (a *agent) hello() wire.Hello {
	var profiles []wire.ProfileState
	ps, err := a.chrome.Profiles()
	if err != nil {
		logf("profiles: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range ps {
		st := wire.ProfileState{Directory: p.Directory, Email: p.Email, Online: true}
		if run, ok := a.running[p.Directory]; ok {
			st.RunningTaskID = run.taskID
		}
		profiles = append(profiles, st)
	}
	return wire.Hello{AgentID: a.id, Version: agentVersion, Profiles: profiles}
}

func (a *agent) heartbeat(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			var running []wire.RunningTask
			for _, r := range a.running {
				running = append(running, wire.RunningTask{TaskID: r.taskID, Attempt: r.attempt, Status: wire.StatusAssigned})
			}
			a.mu.Unlock()
			if err := a.send(conn, wire.MsgHeartbeat, wire.Heartbeat{Running: running}); err != nil {
				return
			}
		}
	}
}

func (a *agent) onMessage(ctx context.Context, conn *websocket.Conn, env wire.Envelope) {
	switch env.Type {
	case wire.MsgAssign:
		var msg wire.Assign
		if err := unmarshal(env, &msg); err != nil {
			logf("assign: %v", err)
			return
		}
		if reason := a.reserve(ctx, msg); reason != "" {
			logf("reject %s: %s", msg.Task.TaskID, reason)
			_ = a.send(conn, wire.MsgReject, wire.Reject{TaskRef: wire.TaskRef{TaskID: msg.Task.TaskID, Attempt: msg.Task.Attempt}, Reason: reason})
		}
	case wire.MsgCancel:
		var msg wire.Cancel
		if err := unmarshal(env, &msg); err != nil {
			logf("cancel: %v", err)
			return
		}
		a.cancel(msg.TaskID)
	case wire.MsgDrain:
		var msg wire.Drain
		if err := unmarshal(env, &msg); err != nil {
			logf("drain: %v", err)
			return
		}
		a.mu.Lock()
		a.drain = msg.Enabled
		a.mu.Unlock()
		logf("drain=%v", msg.Enabled)
	}
}

// reserve starts the task or returns why it was refused.
func (a *agent) reserve(ctx context.Context, msg wire.Assign) string {
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
	if _, ok := a.running[task.ProfileDirectory]; ok {
		a.mu.Unlock()
		return "profile is busy"
	}
	sessCtx, cancel := context.WithCancel(ctx)
	a.running[task.ProfileDirectory] = &runningTask{taskID: task.TaskID, attempt: task.Attempt, cancel: cancel}
	a.mu.Unlock()

	go a.execute(sessCtx, cancel, msg)
	return ""
}

func (a *agent) execute(ctx context.Context, cancel context.CancelFunc, msg wire.Assign) {
	defer cancel()
	defer a.release(msg.Task.ProfileDirectory, msg.Task.TaskID)
	task := msg.Task
	timeout := a.timeout
	if !task.Deadline.IsZero() {
		if d := time.Until(task.Deadline); d < timeout {
			timeout = d
		}
	}
	if timeout <= 0 {
		logf("task %s deadline already passed", task.TaskID)
		return
	}
	sessionDir := filepath.Join(a.work, task.TaskID)
	uploadDir := filepath.Join(sessionDir, "upload")
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		a.fail(task, err)
		return
	}
	videoName := "video" + cleanExt(task.FileExt, task.FileURL)
	if err := downloadFile(ctx, task.FileURL, filepath.Join(uploadDir, videoName), task.SHA256); err != nil {
		a.fail(task, err)
		return
	}
	if task.ThumbnailURL != "" {
		thumb := "thumbnail" + cleanExt("", task.ThumbnailURL)
		if err := downloadFile(ctx, task.ThumbnailURL, filepath.Join(uploadDir, thumb), ""); err != nil {
			a.fail(task, err)
			return
		}
	}
	port, err := session.FreePort()
	if err != nil {
		a.fail(task, err)
		return
	}
	debugPort, err := session.FreePort()
	if err != nil {
		a.fail(task, err)
		return
	}
	chrome, err := a.chrome.Isolated(ctx, task.ProfileDirectory, debugPort)
	if err != nil {
		a.fail(task, err)
		return
	}
	logf("task %s profile %s bmcp %d debug %d runner %s", task.TaskID, task.ProfileDirectory, port, chrome.DebugPort, a.runner)
	began := time.Now()
	sum := runUpload(ctx, uploadRun{
		ID: task.TaskID, Profile: task.ProfileDirectory, Chrome: chrome, Port: port,
		SessionDir: sessionDir, UploadDir: uploadDir,
		Task:   session.Endpoint{URL: msg.TaskMCP.URL, Token: msg.TaskMCP.Token},
		Report: session.Endpoint{URL: msg.ReportMCP.URL, Token: msg.ReportMCP.Token},
		Spec:   a.spec, BMCP: a.bmcp, Launcher: a.launcher, Timeout: timeout,
		Runner: a.runner, FailAt: a.failAt,
	})
	sum.Out.Duration = time.Since(began)
	a.sessionEnded(task, sum)
	if sum.Err != nil {
		logf("task %s: %v", task.TaskID, sum.Err)
		return
	}
	logf("task %s ended: runner=%s failed_steps=%v exit=%d killed_by=%q duration=%s", task.TaskID, sum.Runner, sum.FailedSteps, sum.Out.ExitCode, sum.Out.KilledBy, sum.Out.Duration.Round(time.Second))
}

// sessionEnded tells the server the harness exited, so it can tell a session
// that called task_finish from one that stopped without it.
func (a *agent) sessionEnded(task wire.TaskSpec, sum uploadSummary) {
	out, runErr := sum.Out, sum.Err
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	data := map[string]any{
		"exit_code": out.ExitCode, "killed_by": out.KilledBy, "duration_ms": out.Duration.Milliseconds(),
		"runner": sum.Runner, "failed_steps": sum.FailedSteps, "handoffs": sum.Handoffs,
	}
	if runErr != nil {
		data["error"] = runErr.Error()
	}
	_ = a.send(conn, wire.MsgEvent, wire.Event{
		TaskRef: wire.TaskRef{TaskID: task.TaskID, Attempt: task.Attempt},
		Type:    wire.EventSessionEnded, Data: data, At: time.Now(),
	})
}

func (a *agent) fail(task wire.TaskSpec, err error) {
	logf("task %s: %v", task.TaskID, err)
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	_ = a.send(conn, wire.MsgReject, wire.Reject{
		TaskRef: wire.TaskRef{TaskID: task.TaskID, Attempt: task.Attempt},
		Reason:  err.Error(),
	})
}

func (a *agent) release(profile, taskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if run, ok := a.running[profile]; ok && run.taskID == taskID {
		delete(a.running, profile)
	}
}

func (a *agent) cancel(taskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, run := range a.running {
		if run.taskID == taskID {
			logf("cancel %s", taskID)
			run.cancel()
			return
		}
	}
}

func (a *agent) stopAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, run := range a.running {
		run.cancel()
	}
}

func (a *agent) send(conn *websocket.Conn, typ string, data any) error {
	env, err := wire.NewEnvelope(typ, data)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	return conn.WriteJSON(env)
}

func unmarshal(env wire.Envelope, dest any) error {
	if len(env.Data) == 0 {
		return errors.New("empty data")
	}
	return json.Unmarshal(env.Data, dest)
}

func cleanExt(ext, rawURL string) string {
	if ext == "" {
		ext = filepath.Ext(strings.Split(rawURL, "?")[0])
	}
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

func downloadFile(ctx context.Context, src, dst, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", src, resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(sum, wantSHA) {
		os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: got %s want %s", sum, wantSHA)
	}
	return os.Rename(tmp, dst)
}
