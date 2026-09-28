package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/harness"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/prompt"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/session"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// try runs one upload end to end on this machine without the server: a
// local task MCP serves a single task, and the configured LLM CLI uploads it
// through bmcp. Everything the LLM reports is printed.
func try(args []string) error {
	fs := flag.NewFlagSet("try", flag.ExitOnError)
	preset := fs.String("harness", "claude", "harness preset: claude, cursor, codex, hermes, custom")
	bin := fs.String("bin", "", "override the CLI binary")
	model := fs.String("model", "", "override the model")
	bmcp := fs.String("bmcp", defaultBMCP(), "path to bmcp dist/index.js")
	port := fs.Int("port", 9009, "bmcp WebSocket port the profile's extension connects to")
	video := fs.String("video", "", "video file to upload (required)")
	thumb := fs.String("thumbnail", "", "thumbnail image")
	channel := fs.String("channel", "", "YouTube channel id UC... (required)")
	title := fs.String("title", "", "title (default: file name)")
	desc := fs.String("description", "", "description")
	visibility := fs.String("visibility", "private", "public, unlisted or private")
	playlists := fs.String("playlists", "", "comma-separated playlist names")
	tags := fs.String("tags", "", "comma-separated tags")
	existing := fs.String("existing-video", "", "video id created by an earlier attempt (tests the no-duplicate path)")
	idle := fs.Duration("idle-timeout", 5*time.Minute, "kill the session if it makes no uploader call for this long")
	timeout := fs.Duration("timeout", 45*time.Minute, "kill the session after this long")
	cancelAfter := fs.Duration("cancel-after", 0, "simulate a cancel after this long (tests control=stop and kill)")
	workDir := fs.String("work", "./data/try", "where session files, the upload dir and transcripts go")
	profile := fs.String("profile", "", "Chrome profile directory to use, e.g. Default or \"Profile 1\" (required)")
	userDataDir := fs.String("user-data-dir", defaultUserDataDir(), "Chrome user-data-dir holding the profiles")
	debugPort := fs.Int("debug-port", 9222, "Chrome remote debugging port")
	extDir := fs.String("extension", defaultExtensionDir(), "unpacked Browser MCP extension directory")
	chromeBin := fs.String("chrome-bin", "google-chrome", "Chrome binary")
	_ = fs.Parse(args)

	if *video == "" || *channel == "" || *profile == "" {
		fs.Usage()
		return errors.New("-video, -channel and -profile are required")
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
	if *title == "" {
		*title = strings.TrimSuffix(filepath.Base(*video), filepath.Ext(*video))
	}

	sessionID := randHex(8)
	sessionDir, _ := filepath.Abs(filepath.Join(*workDir, sessionID))
	uploadDir := filepath.Join(sessionDir, "upload")
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return err
	}
	// bmcp resolves symlinks and refuses files outside its upload dir, so the
	// file must really be inside it: hard link, or copy across filesystems.
	videoName := "video" + filepath.Ext(*video)
	if err := linkOrCopy(*video, filepath.Join(uploadDir, videoName)); err != nil {
		return err
	}
	thumbName := ""
	if *thumb != "" {
		thumbName = "thumbnail" + filepath.Ext(*thumb)
		if err := linkOrCopy(*thumb, filepath.Join(uploadDir, thumbName)); err != nil {
			return err
		}
	}

	meta := wire.Metadata{
		Title: *title, Description: *desc, Visibility: wire.Visibility(*visibility),
		Tags: splitList(*tags), Playlists: splitList(*playlists),
	}
	be := newLocalBackend(sessionID, randHex(24), &taskmcp.ClaimOut{
		Control: taskmcp.Continue, Kind: taskmcp.KindUpload, TaskID: "try-" + sessionID, Attempt: 1,
		FilePath: videoName, ThumbnailPath: thumbName, ChannelID: *channel, Metadata: &meta, ProfileDirectory: *profile,
		ExistingVideoID: *existing,
	})

	taskLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	reportLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		taskLn.Close()
		return err
	}
	taskSrv := &http.Server{Handler: taskmcp.NewTaskHandler(be)}
	reportSrv := &http.Server{Handler: taskmcp.NewReportHandler(be)}
	go func() { _ = taskSrv.Serve(taskLn) }()
	go func() { _ = reportSrv.Serve(reportLn) }()
	defer taskSrv.Close()
	defer reportSrv.Close()

	spec, err := harness.Resolve(harness.Spec{Preset: *preset, Bin: *bin, Model: *model})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var cancelC chan struct{}
	if *cancelAfter > 0 {
		cancelC = make(chan struct{})
		go func() {
			t := time.NewTimer(*cancelAfter)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
				close(cancelC)
			}
		}()
	}
	out, err := session.Run(ctx, session.Config{
		ID: sessionID, Dir: sessionDir, UploadDir: uploadDir, Prompt: prompt.Upload,
		Spec: spec, BMCP: *bmcp, Port: *port, Launcher: self,
		ChromeEnv: map[string]string{
			envChromeBin: *chromeBin, envUserDataDir: absUDD, envDebugPort: fmt.Sprint(*debugPort),
			envExtensionDir: absExt, envProfileDir: *profile, envWSPort: fmt.Sprint(*port),
		},
		Task:    session.Endpoint{URL: "http://" + taskLn.Addr().String() + "/mcp", Token: be.token},
		Report:  session.Endpoint{URL: "http://" + reportLn.Addr().String() + "/mcp", Token: be.token},
		Timeout: *timeout, Idle: *idle, IdleFor: be.idleFor,
		Cancel: cancelC, OnCancel: be.requestStop,
		OnLine: func(ev map[string]any) { logToolUse(ev) }, Logf: logf,
	})
	if err != nil {
		return err
	}
	fin := be.finished()
	logf("session ended: exit=%d signaled=%v killed_by=%q turns=%d cost=$%.4f duration=%s",
		out.ExitCode, out.Signaled, out.KilledBy, out.NumTurns, out.CostUSD, out.Duration.Round(time.Second))
	switch {
	case fin == nil:
		logf("RESULT: no task_finish call -> would be FAILED AGENT_NO_RESULT")
	case fin.Status == "done":
		verified, note := verifyVideo(be.videoID(), fin.VideoID, meta.Visibility)
		logf("RESULT: done video=%s verified=%v (%s)", fin.VideoID, verified, note)
	default:
		logf("RESULT: %s code=%s reason=%s", fin.Status, fin.ErrorCode, fin.Reason)
	}
	return nil
}

// localBackend serves exactly one task to one session.
type localBackend struct {
	sessionID, token string
	task             *taskmcp.ClaimOut

	mu       sync.Mutex
	last     time.Time
	stop     bool
	video    string
	finishIn *taskmcp.FinishIn
}

func newLocalBackend(sessionID, token string, task *taskmcp.ClaimOut) *localBackend {
	return &localBackend{sessionID: sessionID, token: token, task: task, last: time.Now()}
}

func (b *localBackend) touch() taskmcp.Control {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = time.Now()
	if b.stop || b.finishIn != nil {
		return taskmcp.Stop
	}
	return taskmcp.Continue
}

func (b *localBackend) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.last)
}

func (b *localBackend) requestStop() { b.mu.Lock(); b.stop = true; b.mu.Unlock() }

func (b *localBackend) videoID() string { b.mu.Lock(); defer b.mu.Unlock(); return b.video }

func (b *localBackend) finished() *taskmcp.FinishIn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.finishIn
}

func (b *localBackend) Authenticate(_ context.Context, token string) (*taskmcp.Session, error) {
	if !taskmcp.TokenEqual(token, b.token) {
		return nil, taskmcp.ErrUnauthorized
	}
	return &taskmcp.Session{ID: b.sessionID}, nil
}

func (b *localBackend) Claim(context.Context, *taskmcp.Session) (*taskmcp.ClaimOut, error) {
	c := b.touch()
	logf("task_claim -> %s", b.task.TaskID)
	if c == taskmcp.Stop {
		return &taskmcp.ClaimOut{Control: taskmcp.Stop, Message: "task cancelled"}, nil
	}
	return b.task, nil
}

func (b *localBackend) checkTask(id string) error {
	if id != b.task.TaskID {
		return fmt.Errorf("unknown task_id %q, this session's task is %q", id, b.task.TaskID)
	}
	return nil
}

func (b *localBackend) Report(_ context.Context, _ *taskmcp.Session, in taskmcp.ReportIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	c := b.touch()
	logf("task_report %-16s %3d%%  %s", in.Step, in.Progress, in.Message)
	return ack(c), nil
}

func (b *localBackend) VideoCreated(_ context.Context, _ *taskmcp.Session, in taskmcp.VideoCreatedIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	c := b.touch()
	b.mu.Lock()
	b.video = in.VideoID
	b.mu.Unlock()
	logf("task_video_created %s %s", in.VideoID, in.VideoURL)
	return ack(c), nil
}

func (b *localBackend) Finish(_ context.Context, _ *taskmcp.Session, in taskmcp.FinishIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	b.touch()
	b.mu.Lock()
	if b.finishIn == nil {
		b.finishIn = &in
	}
	b.mu.Unlock()
	logf("task_finish %s video=%s code=%s reason=%s", in.Status, in.VideoID, in.ErrorCode, in.Reason)
	return &taskmcp.Ack{Control: taskmcp.Stop, Message: "recorded; end the session now"}, nil
}

func ack(c taskmcp.Control) *taskmcp.Ack {
	a := &taskmcp.Ack{Control: c}
	if c == taskmcp.Stop {
		a.Message = "task cancelled: stop now and end the session"
	}
	return a
}

// verifyVideo does not trust the LLM: the id must match the one reported
// when the video was created, and public/unlisted videos must be reachable.
func verifyVideo(created, finished string, vis wire.Visibility) (bool, string) {
	if finished == "" {
		return false, "no video_id in task_finish"
	}
	if created != "" && created != finished {
		return false, fmt.Sprintf("task_finish video %s differs from created video %s", finished, created)
	}
	if vis == wire.VisibilityPrivate {
		return false, "private video: not publicly checkable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape("https://www.youtube.com/watch?v="+finished)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "oembed: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, "oembed ok"
	}
	return false, "oembed " + resp.Status
}

func logToolUse(ev map[string]any) {
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

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func logf(format string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// defaultUserDataDir is uploader/profile, next to resource/. The server lists
// the same folder (-profiles profile), so both see the same Chrome profiles.
func defaultUserDataDir() string {
	return filepath.Join(filepath.Dir(resourceDir()), "profile")
}

// resourceDir is uploader/resource, next to the launcher binary when it was
// built in this repo, or ./resource when started with go run.
func resourceDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "resource")
		if st, err := os.Stat(filepath.Join(dir, "bmcp", "dist", "index.js")); err == nil && !st.IsDir() {
			return dir
		}
	}
	abs, err := filepath.Abs("resource")
	if err == nil {
		if st, err := os.Stat(filepath.Join(abs, "bmcp", "dist", "index.js")); err == nil && !st.IsDir() {
			return abs
		}
	}
	return "resource"
}

func defaultExtensionDir() string {
	return filepath.Join(resourceDir(), "browsermcp-extension")
}

func defaultBMCP() string {
	return filepath.Join(resourceDir(), "bmcp", "dist", "index.js")
}
