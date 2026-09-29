package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/cdp"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/chromectl"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/driver"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/harness"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/playbook"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/prompt"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/session"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// Runners.
const (
	runnerPlaybook = "playbook"     // the script did everything
	runnerMixed    = "playbook+llm" // the script handed steps to an LLM
	runnerLLM      = "llm"          // an LLM session did the task
)

// uploadRun is one attempt of an upload task on one Chrome profile. Everything
// in it belongs to this attempt (its own Chrome, bmcp port, driver and
// playbook state), so attempts on different profiles never share anything.
type uploadRun struct {
	ID         string // task id, used in logs
	Profile    string
	Chrome     *chromectl.Controller // this profile's own Chrome (Isolated)
	Port       int                   // the extension's WebSocket port for this attempt
	SessionDir string
	UploadDir  string
	Task       session.Endpoint
	Report     session.Endpoint
	Spec       harness.Spec
	BMCP       string
	Launcher   string
	Timeout    time.Duration
	Idle       time.Duration
	IdleFor    func() time.Duration
	Cancel     <-chan struct{} // asks the attempt to stop (try -cancel-after)
	OnCancel   func()

	Runner      string // runnerPlaybook (default) or runnerLLM
	FailAt      string // playbook step to fail once, to test the hand-over
	MaxHandoffs int    // step hand-overs before the LLM finishes the task
}

type uploadSummary struct {
	Runner      string
	FailedSteps []string
	Handoffs    int
	Out         session.Outcome // the last LLM session, zero when none ran
	Err         error
}

// runUpload does the attempt: the playbook first, an LLM for a step the
// script cannot do, and an LLM for the rest when a hand-over does not help.
func runUpload(ctx context.Context, u uploadRun) uploadSummary {
	start := time.Now()
	deadline := start.Add(u.Timeout)
	remaining := func() time.Duration { return time.Until(deadline) }
	sum := uploadSummary{Runner: runnerPlaybook}
	if u.MaxHandoffs <= 0 {
		u.MaxHandoffs = 2
	}

	if u.Runner == runnerLLM {
		sum.Runner = runnerLLM
		sum.Out, sum.Err = u.llm(ctx, u.SessionDir, prompt.Upload, nil, remaining(), 0)
		return sum
	}

	client := taskmcp.NewClient(u.Task.URL, u.Task.Token, u.Report.URL, u.Report.Token)
	claim, err := client.Claim(ctx)
	switch {
	case err != nil:
		logf("task %s: playbook claim: %v; running the LLM", u.ID, err)
		sum.Runner = runnerLLM
		sum.Out, sum.Err = u.llm(ctx, u.SessionDir, prompt.Upload, nil, remaining(), 0)
		return sum
	case claim.Control == taskmcp.Stop || claim.TaskID == "":
		logf("task %s: nothing to do (%s)", u.ID, claim.Message)
		return sum
	case claim.Kind != taskmcp.KindUpload || claim.Metadata == nil:
		// Deleting videos is LLM work.
		sum.Runner = runnerLLM
		sum.Out, sum.Err = u.llm(ctx, u.SessionDir, prompt.Upload, nil, remaining(), 0)
		return sum
	}

	task := playbook.Task{
		ExistingVideoID: claim.ExistingVideoID,
		ChannelID:       claim.ChannelID,
		VideoPath:       filepath.Join(u.UploadDir, claim.FilePath),
		Meta:            *claim.Metadata,
	}
	if claim.ThumbnailPath != "" {
		task.ThumbPath = filepath.Join(u.UploadDir, claim.ThumbnailPath)
	}
	r := playbook.New(task, &reporter{c: client, taskID: claim.TaskID}, func(f string, a ...any) {
		logf("task %s: "+f, append([]any{u.ID}, a...)...)
	})
	r.FailAt = u.FailAt

	for first := true; ; first = false {
		res, snap := u.drive(ctx, r, task, first)
		if res.Outcome != playbook.Failed {
			logf("task %s: playbook ended (%v) after %s", u.ID, res.Outcome, time.Since(start).Round(time.Second))
			return sum
		}
		if ctx.Err() != nil {
			sum.Err = ctx.Err()
			return sum
		}
		stepName := "?"
		if res.Step != nil {
			stepName = res.Step.Name
		}
		if n := len(sum.FailedSteps); n == 0 || sum.FailedSteps[n-1] != stepName {
			sum.FailedSteps = append(sum.FailedSteps, stepName)
		}
		sum.Runner = runnerMixed
		sc := prompt.StepContext{
			TaskID: claim.TaskID, Step: stepName, URL: res.URL, VideoID: r.VideoID(),
			Done: r.DoneSteps(), Title: task.Meta.Title, Channel: task.ChannelID,
			Kids: task.Meta.MadeForKids, Visible: string(task.Meta.Visibility), Snapshot: snap,
			Attached: r.Attached(),
		}
		if res.Err != nil {
			sc.Error = res.Err.Error()
		}
		if res.Step != nil {
			sc.Goal = res.Step.Goal
		}

		if res.Fatal || res.Step == nil || res.Step.Check == "" || sum.Handoffs >= u.MaxHandoffs {
			logf("task %s: step %s cannot be handed over on its own (%v); the LLM finishes the task", u.ID, stepName, res.Err)
			u.hideAttachedVideo(r, task)
			sum.Out, sum.Err = u.llm(ctx, u.SessionDir, prompt.Handoff(sc), nil, remaining(), 0)
			return sum
		}

		sum.Handoffs++
		r.HandedOff(stepName)
		dir := filepath.Join(u.SessionDir, fmt.Sprintf("step-%d-%s", sum.Handoffs, stepName))
		logf("task %s: handing step %s to the LLM (%s)", u.ID, stepName, dir)
		budget := min(remaining(), 10*time.Minute)
		out, reached, err := u.fixStep(ctx, dir, prompt.Step(sc), res.Step.Check, budget)
		sum.Out = out
		if err != nil {
			logf("task %s: step session: %v", u.ID, err)
		}
		if ctx.Err() != nil {
			// Cancelled or the launcher is stopping: nothing to resume.
			sum.Err = ctx.Err()
			return sum
		}
		logf("task %s: step %s session ended (goal reached: %v); the script checks the page and goes on", u.ID, stepName, reached)
		if remaining() <= 0 {
			sum.Err = errors.New("task deadline passed")
			return sum
		}
	}
}

// drive runs the playbook with a driver listening on the attempt's port. The
// listener is closed before returning, so an LLM session's bmcp can take
// the same port next.
func (u uploadRun) drive(ctx context.Context, r *playbook.Runner, task playbook.Task, first bool) (playbook.Result, string) {
	failed := func(err error) (playbook.Result, string) {
		return playbook.Result{Outcome: playbook.Failed, Step: r.Current(), Err: err, Fatal: true}, ""
	}
	dctx, cancel := context.WithCancel(ctx)
	srv := driver.NewServer(fmt.Sprintf("127.0.0.1:%d", u.Port))
	done := make(chan struct{})
	var lerr error
	go func() { lerr = srv.ListenAndServe(dctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-done:
		return failed(fmt.Errorf("driver on port %d: %w", u.Port, lerr))
	case <-time.After(300 * time.Millisecond):
	}

	if first {
		if _, err := u.Chrome.Open(dctx, u.Profile); err != nil {
			return failed(fmt.Errorf("chrome_open: %w", err))
		}
		if _, err := u.Chrome.Setup(dctx, u.Profile, u.Port, task.StartURL()); err != nil {
			return failed(fmt.Errorf("extension_setup: %w", err))
		}
	}
	conn, err := waitExtension(dctx, srv, 30*time.Second)
	if err != nil && !first {
		// The LLM session may have moved the extension; point it back here.
		if _, serr := u.Chrome.Setup(dctx, u.Profile, u.Port, task.StartURL()); serr == nil {
			conn, err = waitExtension(dctx, srv, 30*time.Second)
		}
	}
	if err != nil {
		return failed(fmt.Errorf("extension did not connect on port %d: %w", u.Port, err))
	}
	b := srv.Browser(conn.Hello().InstanceID)
	if !first {
		// Back on the upload dialog's tab, whatever the LLM had selected.
		if _, err := b.SelectTab(dctx, "/videos/upload"); err != nil {
			_, _ = b.SelectTab(dctx, "studio.youtube.com")
		}
	}
	res := r.Run(dctx, b)
	if res.Outcome != playbook.Failed {
		return res, ""
	}
	snap, _ := b.Snapshot(dctx)
	name := "unknown"
	if res.Step != nil {
		name = res.Step.Name
	}
	base := filepath.Join(u.SessionDir, "playbook-"+name)
	if snap != "" {
		_ = os.WriteFile(base+".txt", []byte(snap), 0o600)
	}
	if png, err := b.Screenshot(dctx); err == nil {
		_ = os.WriteFile(base+".png", png, 0o600)
	}
	return res, snap
}

func waitExtension(ctx context.Context, srv *driver.Server, d time.Duration) (*driver.Conn, error) {
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return srv.WaitFor(wctx, func(driver.Hello) bool { return true })
}

// fixStep runs an LLM session for one step and stops it as soon as the
// step's check holds on the page, read over CDP on this profile's own
// Chrome, so a model that forgets to end does not hold the upload up.
func (u uploadRun) fixStep(ctx context.Context, dir, text, check string, budget time.Duration) (session.Outcome, bool, error) {
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	reachedC := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				if pageCheck(wctx, u.Chrome.DebugPort, check) {
					logf("task %s: step goal reached on the page; stopping the LLM session", u.ID)
					once.Do(func() { close(reachedC) })
					return
				}
			}
		}
	}()
	out, err := u.llm(ctx, dir, text, reachedC, budget, 3*time.Second)
	select {
	case <-reachedC:
		return out, true, err
	default:
		return out, pageCheck(ctx, u.Chrome.DebugPort, check), err
	}
}

// pageCheck evaluates expr in the YouTube Studio tabs of the profile's
// Chrome; true if it holds in one of them.
func pageCheck(ctx context.Context, port int, expr string) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := cdp.Dial(cctx, port)
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
		err = conn.Eval(cctx, s, expr, &ok)
		conn.Detach(cctx, s)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// llm runs one LLM session. extraCancel stops it early (goal reached);
// u.Cancel still works and is the only one that runs OnCancel.
func (u uploadRun) llm(ctx context.Context, dir, text string, extraCancel <-chan struct{}, timeout, grace time.Duration) (session.Outcome, error) {
	if timeout <= 0 {
		return session.Outcome{}, errors.New("task deadline passed")
	}
	cancelC := u.Cancel
	onCancel := u.OnCancel
	if extraCancel != nil {
		merged := make(chan struct{})
		stopMerge := make(chan struct{})
		defer close(stopMerge)
		go func() {
			select {
			case <-u.Cancel:
			case <-extraCancel:
			case <-stopMerge:
				return
			}
			close(merged)
		}()
		cancelC = merged
		onCancel = func() {
			select {
			case <-u.Cancel:
				if u.OnCancel != nil {
					u.OnCancel()
				}
			default:
			}
		}
	}
	return session.Run(ctx, session.Config{
		ID: u.ID, Dir: dir, UploadDir: u.UploadDir, Prompt: text,
		Spec: u.Spec, BMCP: u.BMCP, Port: u.Port, Launcher: u.Launcher,
		ChromeEnv: map[string]string{
			envChromeBin: u.Chrome.Bin, envUserDataDir: u.Chrome.UserDataDir,
			envDebugPort: fmt.Sprint(u.Chrome.DebugPort), envExtensionDir: u.Chrome.ExtensionDir,
			envProfileDir: u.Profile, envWSPort: fmt.Sprint(u.Port),
		},
		Task: u.Task, Report: u.Report,
		Timeout: timeout, Idle: u.Idle, IdleFor: u.IdleFor,
		Cancel: cancelC, OnCancel: onCancel, Grace: grace,
		OnLine: func(ev map[string]any) { logToolUse(ev) }, Logf: logf,
	})
}

// hideAttachedVideo moves the video out of the upload dir once the script
// attached it, so the LLM finishing the task cannot upload a second copy.
func (u uploadRun) hideAttachedVideo(r *playbook.Runner, task playbook.Task) {
	if !r.Attached() {
		return
	}
	dst := filepath.Join(u.SessionDir, "attached-"+filepath.Base(task.VideoPath))
	if err := os.Rename(task.VideoPath, dst); err != nil {
		logf("task %s: hide attached video: %v", u.ID, err)
	}
}

// reporter is the playbook's view of task_mcp / report_mcp.
type reporter struct {
	c      *taskmcp.Client
	taskID string
}

func (r *reporter) Report(ctx context.Context, step wire.Status, msg string, progress int) (bool, error) {
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

func (r *reporter) Finish(ctx context.Context, status, code, reason, id string) error {
	_, err := r.c.Finish(ctx, taskmcp.FinishIn{TaskID: r.taskID, Status: status, ErrorCode: code, Reason: reason, VideoID: id})
	return err
}
