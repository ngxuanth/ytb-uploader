// Package studioenv is the upload attempt's environment on this machine:
// the profile's own Chrome, the Browser MCP extension driven by the Studio
// playbook, LLM sessions through the harness, and task_mcp / report_mcp
// over MCP. It implements the ports of internal/app/upload.
package studioenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/chromemcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/chrome"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/chrome/cdp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/extension"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/harness"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/prompt"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/session"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/studio"
)

// Env is one attempt's environment. Everything in it belongs to this
// attempt (its own Chrome, bmcp port, driver and playbook state), so
// attempts on different profiles never share anything.
type Env struct {
	ID         string // task id, used in logs
	Profile    string
	Chrome     *chrome.Controller // this profile's own Chrome (Isolated)
	Port       int                // the extension's WebSocket port for this attempt
	SessionDir string
	UploadDir  string
	Task       session.Endpoint
	Report     session.Endpoint
	Spec       harness.Spec
	BMCP       string
	Launcher   string
	Idle       time.Duration
	IdleFor    func() time.Duration
	Cancel     <-chan struct{} // asks the attempt to stop (try -cancel-after)
	OnCancel   func()
	FailAt     string // playbook step to fail once, to test the hand-over
	// StateAfterUpload reads the video's state in Studio after the save and
	// reports it with task_video_state.
	StateAfterUpload bool
	Logf             func(string, ...any)

	client *taskmcp.Client
}

var _ upload.Env = (*Env)(nil)

func (e *Env) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

func (e *Env) taskClient() *taskmcp.Client {
	if e.client == nil {
		e.client = taskmcp.NewClient(e.Task.URL, e.Task.Token, e.Report.URL, e.Report.Token)
	}
	return e.client
}

// Tasks is task_mcp / report_mcp for this attempt.
func (e *Env) Tasks() upload.Tasks { return tasks{e.taskClient()} }

func (e *Env) runner(task studio.Task, taskID string) *studio.Runner {
	return studio.New(task, &reporter{c: e.taskClient(), taskID: taskID}, func(f string, a ...any) {
		e.logf("task %s: "+f, append([]any{e.ID}, a...)...)
	})
}

// Script is the Studio playbook for the claimed upload.
func (e *Env) Script(c upload.Claim) upload.Script {
	task := studio.Task{
		ExistingVideoID: c.ExistingVideoID,
		ChannelID:       c.ChannelID,
		VideoPath:       filepath.Join(e.UploadDir, c.FilePath),
	}
	if c.Metadata != nil {
		task.Meta = *c.Metadata
	}
	if c.ThumbnailPath != "" {
		task.ThumbPath = filepath.Join(e.UploadDir, c.ThumbnailPath)
	}
	if fi, err := os.Stat(task.VideoPath); err == nil {
		task.VideoSize = fi.Size()
	}
	r := e.runner(task, c.TaskID)
	r.FailAt = e.FailAt
	r.StateAfterUpload = e.StateAfterUpload
	return &script{e: e, r: r, task: task}
}

// CheckVideo reads the video's state in Studio and reports it.
func (e *Env) CheckVideo(ctx context.Context, c upload.Claim) error {
	task := studio.Task{ExistingVideoID: c.ExistingVideoID, ChannelID: c.ChannelID}
	r := e.runner(task, c.TaskID)
	return e.withDriver(ctx, task, func(b *extension.Browser) error { return r.Check(ctx, b) })
}

// withDriver opens this profile's Chrome, points the extension at a driver
// on the attempt's port and runs fn with the connected browser.
func (e *Env) withDriver(ctx context.Context, task studio.Task, fn func(*extension.Browser) error) error {
	dctx, cancel := context.WithCancel(ctx)
	srv := extension.NewServer(fmt.Sprintf("127.0.0.1:%d", e.Port))
	done := make(chan struct{})
	var lerr error
	go func() { lerr = srv.ListenAndServe(dctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-done:
		return fmt.Errorf("driver on port %d: %w", e.Port, lerr)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := e.Chrome.Open(dctx, e.Profile); err != nil {
		return fmt.Errorf("chrome_open: %w", err)
	}
	if _, err := e.Chrome.Setup(dctx, e.Profile, e.Port, task.StartURL()); err != nil {
		return fmt.Errorf("extension_setup: %w", err)
	}
	conn, err := waitExtension(dctx, srv, 30*time.Second)
	if err != nil {
		return fmt.Errorf("extension did not connect on port %d: %w", e.Port, err)
	}
	return fn(srv.Browser(conn.Hello().InstanceID))
}

func waitExtension(ctx context.Context, srv *extension.Server, d time.Duration) (*extension.Conn, error) {
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return srv.WaitFor(wctx, func(extension.Hello) bool { return true })
}

// PageHolds evaluates check in the YouTube Studio tabs of the profile's
// Chrome over CDP; true if it holds in one of them.
func (e *Env) PageHolds(ctx context.Context, check string) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := cdp.Dial(cctx, e.Chrome.DebugPort)
	if err != nil {
		return false
	}
	defer conn.Close()
	ts, err := conn.Targets(cctx)
	if err != nil {
		return false
	}
	for _, t := range ts {
		if t.Type != "page" || !strings.Contains(t.URL, "studio.youtube.com") {
			continue
		}
		s, err := conn.Attach(cctx, t.TargetID)
		if err != nil {
			continue
		}
		var ok bool
		err = conn.Eval(cctx, s, check, &ok)
		conn.Detach(cctx, s)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// LLM runs one LLM session. s.Stop ends it early (goal reached); e.Cancel
// still works and is the only one that runs OnCancel.
func (e *Env) LLM(ctx context.Context, s upload.Session) (upload.Outcome, error) {
	cancelC := e.Cancel
	onCancel := e.OnCancel
	if s.Stop != nil {
		merged := make(chan struct{})
		stopMerge := make(chan struct{})
		defer close(stopMerge)
		go func() {
			select {
			case <-e.Cancel:
			case <-s.Stop:
			case <-stopMerge:
				return
			}
			close(merged)
		}()
		cancelC = merged
		onCancel = func() {
			select {
			case <-e.Cancel:
				if e.OnCancel != nil {
					e.OnCancel()
				}
			default:
			}
		}
	}
	out, err := session.Run(ctx, session.Config{
		ID: e.ID, Dir: s.Dir, UploadDir: e.UploadDir, Prompt: promptText(s),
		Spec: e.Spec, BMCP: e.BMCP, Port: e.Port, Launcher: e.Launcher,
		ChromeEnv: chromemcp.Env(e.Chrome, e.Profile, e.Port),
		Task:      e.Task, Report: e.Report,
		Timeout: s.Timeout, Idle: e.Idle, IdleFor: e.IdleFor,
		Cancel: cancelC, OnCancel: onCancel, Grace: s.Grace,
		OnLine: func(ev map[string]any) { logToolUse(e.logf, ev) }, Logf: e.Logf,
	})
	return upload.Outcome{
		ExitCode: out.ExitCode, Signaled: out.Signaled, KilledBy: out.KilledBy,
		NumTurns: out.NumTurns, CostUSD: out.CostUSD, Duration: out.Duration,
	}, err
}

func promptText(s upload.Session) string {
	c := s.Context
	sc := prompt.StepContext{
		TaskID: c.TaskID, Step: c.Step, Goal: c.Goal, Error: c.Error, URL: c.URL, VideoID: c.VideoID,
		Done: c.Done, Title: c.Title, Channel: c.Channel, Kids: c.Kids, Visible: c.Visible,
		Snapshot: c.Snapshot, Attached: c.Attached,
	}
	switch s.Kind {
	case upload.PromptStep:
		return prompt.Step(sc)
	case upload.PromptHandoff:
		return prompt.Handoff(sc)
	default:
		return prompt.Upload
	}
}

func logToolUse(logf func(string, ...any), ev map[string]any) {
	// Hermes stream-json: one top-level tool_use event per call.
	if ev["type"] == "tool_use" {
		if name, _ := ev["name"].(string); !strings.Contains(name, "task_") {
			logf("  tool %s", name)
		}
		return
	}
	// Claude / Cursor stream-json: assistant messages carry tool_use blocks.
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, c := range content {
		block, _ := c.(map[string]any)
		if block["type"] == "tool_use" {
			name, _ := block["name"].(string)
			if !strings.Contains(name, "task_") {
				logf("  tool %s", name)
			}
		}
	}
}
