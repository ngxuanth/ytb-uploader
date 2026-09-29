package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/agentclient"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/studioenv"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/agent"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/chrome"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/harness"
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
	runner := fs.String("runner", upload.RunnerPlaybook, "playbook: script first, LLM only for failed steps; llm: an LLM session does every task")
	idleClose := fs.Duration("chrome-idle-close", 20*time.Second, "close a profile's Chrome when no task has used it for this long after the last one (0 = keep it open)")
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
	ctl := &chrome.Controller{Bin: *chromeBin, UserDataDir: absUDD, DebugPort: *debugPort, ExtensionDir: absExt}
	exec := &studioenv.Executor{
		Chrome: ctl, Work: absWork, Spec: spec, BMCP: *bmcp, Launcher: self,
		Runner: *runner, FailAt: *failAt, Logf: logf,
	}
	a := &agent.Agent{
		ID: id, Version: agentVersion, Timeout: *timeout, Logf: logf,
		Profiles: func() ([]agent.Profile, error) {
			ps, err := ctl.Profiles()
			out := make([]agent.Profile, len(ps))
			for i, p := range ps {
				out[i] = agent.Profile{Directory: p.Directory, Email: p.Email}
			}
			return out, err
		},
		Exec: exec, Browsers: exec, IdleClose: *idleClose,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return agentclient.Run(ctx, *serverURL, a, logf)
}
