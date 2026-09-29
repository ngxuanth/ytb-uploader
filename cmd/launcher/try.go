package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/localtask"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/studioenv"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/chrome"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/harness"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/session"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/localfs"
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
	channel := fs.String("channel", "", "YouTube channel id UC... (optional; empty uses the profile's default channel)")
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
	runner := fs.String("runner", upload.RunnerPlaybook, "playbook: script first, LLM only for failed steps; llm: LLM session only")
	failAt := fs.String("playbook-fail-at", "", "testing: make this playbook step fail once to exercise the LLM hand-over")
	_ = fs.Parse(args)

	if *video == "" || *profile == "" {
		fs.Usage()
		return errors.New("-video and -profile are required")
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
	if err := localfs.LinkOrCopy(*video, filepath.Join(uploadDir, videoName)); err != nil {
		return err
	}
	thumbName := ""
	if *thumb != "" {
		thumbName = "thumbnail" + filepath.Ext(*thumb)
		if err := localfs.LinkOrCopy(*thumb, filepath.Join(uploadDir, thumbName)); err != nil {
			return err
		}
	}

	meta := contract.Metadata{
		Title: *title, Description: *desc, Visibility: contract.Visibility(*visibility),
		Tags: splitList(*tags), Playlists: splitList(*playlists),
	}
	be := localtask.NewBackend(sessionID, randHex(24), &taskmcp.ClaimOut{
		Control: taskmcp.Continue, Kind: taskmcp.KindUpload, TaskID: "try-" + sessionID, Attempt: 1,
		FilePath: videoName, ThumbnailPath: thumbName, ChannelID: *channel, Metadata: &meta, ProfileDirectory: *profile,
		ExistingVideoID: *existing,
	}, logf)

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
	ctl, err := (&chrome.Controller{Bin: *chromeBin, UserDataDir: absUDD, ExtensionDir: absExt}).Isolated(ctx, *profile, *debugPort)
	if err != nil {
		return err
	}
	env := &studioenv.Env{
		ID: sessionID, Profile: *profile, Chrome: ctl, Port: *port,
		SessionDir: sessionDir, UploadDir: uploadDir,
		Task:   session.Endpoint{URL: "http://" + taskLn.Addr().String() + "/mcp", Token: be.Token()},
		Report: session.Endpoint{URL: "http://" + reportLn.Addr().String() + "/mcp", Token: be.Token()},
		Spec:   spec, BMCP: *bmcp, Launcher: self,
		Idle: *idle, IdleFor: be.IdleFor, Cancel: cancelC, OnCancel: be.RequestStop,
		FailAt: *failAt, Logf: logf,
	}
	sum := upload.Run(ctx, upload.Attempt{
		ID: sessionID, SessionDir: sessionDir, Runner: *runner, Logf: logf,
		Timeout: max(*timeout, contract.UploadBudget(fileSize(*video))),
	}, env.Tasks(), env)
	if sum.Err != nil {
		return sum.Err
	}
	out := sum.Out
	logf("runner=%s failed_steps=%v handoffs=%d", sum.Runner, sum.FailedSteps, sum.Handoffs)
	fin := be.Finished()
	logf("session ended: exit=%d signaled=%v killed_by=%q turns=%d cost=$%.4f duration=%s",
		out.ExitCode, out.Signaled, out.KilledBy, out.NumTurns, out.CostUSD, out.Duration.Round(time.Second))
	switch {
	case fin == nil:
		logf("RESULT: no task_finish call -> would be FAILED AGENT_NO_RESULT")
	case fin.Status == "done":
		verified, note := localtask.VerifyVideo(be.VideoID(), fin.VideoID, meta.Visibility)
		logf("RESULT: done video=%s verified=%v (%s)", fin.VideoID, verified, note)
	default:
		logf("RESULT: %s code=%s reason=%s", fin.Status, fin.ErrorCode, fin.Reason)
	}
	return nil
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

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
