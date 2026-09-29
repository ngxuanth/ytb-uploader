package studioenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/agent"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/chrome"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/download"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/harness"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/session"
)

// Executor prepares an assigned attempt on this machine (files, ports, the
// profile's own Chrome) and runs it.
type Executor struct {
	Chrome   *chrome.Controller // the shared user-data-dir; each attempt gets an isolated one
	Work     string             // session dirs go here, one per task
	Spec     harness.Spec
	BMCP     string
	Launcher string
	Runner   string
	FailAt   string
	Logf     func(string, ...any)
}

var (
	_ agent.Executor = (*Executor)(nil)
	_ agent.Browsers = (*Executor)(nil)
)

// Close closes the profile's own Chrome.
func (x *Executor) Close(ctx context.Context, profile string) error {
	return x.Chrome.CloseIsolated(ctx, profile)
}

func (x *Executor) Execute(ctx context.Context, msg contract.Assign, timeout time.Duration) (upload.Summary, error) {
	task := msg.Task
	sessionDir := filepath.Join(x.Work, task.TaskID)
	uploadDir := filepath.Join(sessionDir, "upload")
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return upload.Summary{}, err
	}
	// A check_video task only reads Studio; there is no file.
	if task.Kind != upload.KindCheckVideo {
		videoName := "video" + cleanExt(task.FileExt, task.FileURL)
		if err := download.File(ctx, task.FileURL, filepath.Join(uploadDir, videoName), task.SHA256); err != nil {
			return upload.Summary{}, err
		}
	}
	if task.ThumbnailURL != "" {
		thumb := "thumbnail" + cleanExt("", task.ThumbnailURL)
		if err := download.File(ctx, task.ThumbnailURL, filepath.Join(uploadDir, thumb), ""); err != nil {
			return upload.Summary{}, err
		}
	}
	port, err := session.FreePort()
	if err != nil {
		return upload.Summary{}, err
	}
	debugPort, err := session.FreePort()
	if err != nil {
		return upload.Summary{}, err
	}
	ctl, err := x.Chrome.Isolated(ctx, task.ProfileDirectory, debugPort)
	if err != nil {
		return upload.Summary{}, err
	}
	x.Logf("task %s profile %s bmcp %d debug %d runner %s", task.TaskID, task.ProfileDirectory, port, ctl.DebugPort, x.Runner)
	env := &Env{
		ID: task.TaskID, Profile: task.ProfileDirectory, Chrome: ctl, Port: port,
		SessionDir: sessionDir, UploadDir: uploadDir,
		Task:   session.Endpoint{URL: msg.TaskMCP.URL, Token: msg.TaskMCP.Token},
		Report: session.Endpoint{URL: msg.ReportMCP.URL, Token: msg.ReportMCP.Token},
		Spec:   x.Spec, BMCP: x.BMCP, Launcher: x.Launcher, FailAt: x.FailAt, Logf: x.Logf,
	}
	return upload.Run(ctx, upload.Attempt{
		ID: task.TaskID, SessionDir: sessionDir, Timeout: timeout, Runner: x.Runner, Logf: x.Logf,
	}, env.Tasks(), env), nil
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
