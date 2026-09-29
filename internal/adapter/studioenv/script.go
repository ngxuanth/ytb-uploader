package studioenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/upload"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/extension"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/studio"
)

// script is the Studio playbook as an upload.Script.
type script struct {
	e    *Env
	r    *studio.Runner
	task studio.Task
}

func (s *script) VideoID() string       { return s.r.VideoID() }
func (s *script) Done() []string        { return s.r.DoneSteps() }
func (s *script) Attached() bool        { return s.r.Attached() }
func (s *script) HandedOff(step string) { s.r.HandedOff(step) }

// HideVideo moves the video out of the upload dir once the script attached
// it.
func (s *script) HideVideo() {
	if !s.r.Attached() {
		return
	}
	dst := filepath.Join(s.e.SessionDir, "attached-"+filepath.Base(s.task.VideoPath))
	if err := os.Rename(s.task.VideoPath, dst); err != nil {
		s.e.logf("task %s: hide attached video: %v", s.e.ID, err)
	}
}

// Run runs the playbook with a driver listening on the attempt's port. The
// listener is closed before returning, so an LLM session's bmcp can take
// the same port next.
func (s *script) Run(ctx context.Context, first bool) upload.Result {
	res, snap := s.drive(ctx, first)
	if res.Outcome != studio.Failed {
		return upload.Result{}
	}
	out := upload.Result{Failed: true, Err: res.Err, Fatal: res.Fatal, URL: res.URL, Snapshot: snap}
	if res.Step != nil {
		out.Step = &upload.Step{Name: res.Step.Name, Goal: res.Step.Goal, Check: res.Step.Check}
	}
	return out
}

func (s *script) drive(ctx context.Context, first bool) (studio.Result, string) {
	e, r, task := s.e, s.r, s.task
	failed := func(err error) (studio.Result, string) {
		return studio.Result{Outcome: studio.Failed, Step: r.Current(), Err: err, Fatal: true}, ""
	}
	dctx, cancel := context.WithCancel(ctx)
	srv := extension.NewServer(fmt.Sprintf("127.0.0.1:%d", e.Port))
	done := make(chan struct{})
	var lerr error
	go func() { lerr = srv.ListenAndServe(dctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-done:
		return failed(fmt.Errorf("driver on port %d: %w", e.Port, lerr))
	case <-time.After(300 * time.Millisecond):
	}

	if first {
		if _, err := e.Chrome.Open(dctx, e.Profile); err != nil {
			return failed(fmt.Errorf("chrome_open: %w", err))
		}
		if _, err := e.Chrome.Setup(dctx, e.Profile, e.Port, task.StartURL()); err != nil {
			return failed(fmt.Errorf("extension_setup: %w", err))
		}
	}
	conn, err := waitExtension(dctx, srv, 30*time.Second)
	if err != nil && !first {
		// The LLM session may have moved the extension; point it back here.
		if _, serr := e.Chrome.Setup(dctx, e.Profile, e.Port, task.StartURL()); serr == nil {
			conn, err = waitExtension(dctx, srv, 30*time.Second)
		}
	}
	if err != nil {
		return failed(fmt.Errorf("extension did not connect on port %d: %w", e.Port, err))
	}
	b := srv.Browser(conn.Hello().InstanceID)
	if !first {
		// Back on the upload dialog's tab, whatever the LLM had selected.
		if _, err := b.SelectTab(dctx, "/videos/upload"); err != nil {
			_, _ = b.SelectTab(dctx, "studio.youtube.com")
		}
	}
	res := r.Run(dctx, b)
	if res.Outcome != studio.Failed {
		return res, ""
	}
	snap, _ := b.Snapshot(dctx)
	name := "unknown"
	if res.Step != nil {
		name = res.Step.Name
	}
	base := filepath.Join(e.SessionDir, "playbook-"+name)
	if snap != "" {
		_ = os.WriteFile(base+".txt", []byte(snap), 0o600)
	}
	if png, err := b.Screenshot(dctx); err == nil {
		_ = os.WriteFile(base+".png", png, 0o600)
	}
	return res, snap
}

// tasks is upload.Tasks over the MCP client.
type tasks struct{ c *taskmcp.Client }

func (t tasks) Claim(ctx context.Context) (upload.Claim, error) {
	out, err := t.c.Claim(ctx)
	if err != nil {
		return upload.Claim{}, err
	}
	return upload.Claim{
		Stop: out.Control == taskmcp.Stop, Message: out.Message, Kind: out.Kind, TaskID: out.TaskID,
		ExistingVideoID: out.ExistingVideoID, ChannelID: out.ChannelID,
		FilePath: out.FilePath, ThumbnailPath: out.ThumbnailPath, Metadata: out.Metadata,
	}, nil
}

func (t tasks) Finish(ctx context.Context, taskID, status, code, reason, videoID string) error {
	return (&reporter{c: t.c, taskID: taskID}).Finish(ctx, status, code, reason, videoID)
}

// reporter is the playbook's view of task_mcp / report_mcp.
type reporter struct {
	c      *taskmcp.Client
	taskID string
}

func (r *reporter) Report(ctx context.Context, step contract.Status, msg string, progress int) (bool, error) {
	ack, err := r.c.Report(ctx, taskmcp.ReportIn{TaskID: r.taskID, Step: string(step), Progress: progress, Message: msg})
	if err != nil {
		return false, err
	}
	return ack.Control == taskmcp.Stop, nil
}

func (r *reporter) VideoCreated(ctx context.Context, id, url string) (bool, error) {
	ack, err := r.c.VideoCreated(ctx, taskmcp.VideoCreatedIn{TaskID: r.taskID, VideoID: id, VideoURL: url})
	if err != nil {
		return false, err
	}
	return ack.Control == taskmcp.Stop, nil
}

func (r *reporter) VideoState(ctx context.Context, st contract.VideoState) (bool, error) {
	ack, err := r.c.VideoState(ctx, taskmcp.VideoStateIn{TaskID: r.taskID, VideoState: st})
	if err != nil {
		return false, err
	}
	return ack.Control == taskmcp.Stop, nil
}

func (r *reporter) Finish(ctx context.Context, status, code, reason, id string) error {
	_, err := r.c.Finish(ctx, taskmcp.FinishIn{TaskID: r.taskID, Status: status, ErrorCode: code, Reason: reason, VideoID: id})
	return err
}
