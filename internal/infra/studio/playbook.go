// Package studio uploads a video through the YouTube Studio upload dialog
// without an LLM. Each step checks the page before and after it acts. When
// a step fails the launcher hands just that step to an LLM session, waits
// until the step's Check holds, then calls Run again: finished steps are
// skipped because their Check already holds, and the script carries on.
//
// The package is split by what changes for what reason:
//   - playbook.go  the Runner and the Run state machine (this file)
//   - steps.go     buildSteps and the per-step actions and navigation
//   - selectors.go CSS-selector and URL builders
//   - scripts.go   the JS snippets evaluated on the page
//   - state.go     reading a video's state back from Studio
package studio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Page is what the playbook needs from the browser; *extension.Browser is one.
type Page interface {
	Navigate(ctx context.Context, url string) error
	URL(ctx context.Context) (string, error)
	Evaluate(ctx context.Context, expression string, out any) error
	ClickSelector(ctx context.Context, selector string) error
	TypeSelector(ctx context.Context, selector, text string, submit bool) error
	UploadFile(ctx context.Context, selector, path string) error
	Scroll(ctx context.Context, selector string) error
}

// Reporter is task_mcp / report_mcp as seen by the script. stop is the
// server's control "stop" (task cancelled or already finished).
type Reporter interface {
	Report(ctx context.Context, step contract.Status, message string, progress int) (stop bool, err error)
	VideoCreated(ctx context.Context, videoID, videoURL string) (stop bool, err error)
	Finish(ctx context.Context, status, errorCode, reason, videoID string) error
	VideoState(ctx context.Context, st contract.VideoState) (stop bool, err error)
}

// Task is the part of task_claim the script uses.
type Task struct {
	// ExistingVideoID is a video an earlier attempt (or the script itself)
	// already created: the script reopens its draft instead of uploading.
	ExistingVideoID string
	ChannelID       string // may be empty: the profile's default channel
	VideoPath       string // absolute path Chrome reads the file from
	VideoSize       int64  // bytes; sizes the wait for the upload
	ThumbPath       string
	Meta            contract.Metadata
}

// Step is one stage of the upload dialog.
type Step struct {
	Name   string
	Status contract.Status // reported with task_report when the step starts
	// Goal says in words what must hold after the step; it is the LLM's
	// brief when the step is handed over.
	Goal string
	// Check is a JS expression that is true once the step's outcome holds.
	// The launcher evaluates the same expression over CDP to see when an LLM
	// has finished the step. Empty means the outcome cannot be checked, so
	// the step cannot be handed over on its own.
	Check string
	// Unsupported steps are always handed to the LLM, which then finishes
	// the whole task.
	Unsupported bool
	do          func(ctx context.Context, r *Runner, p Page) error
	skip        func(r *Runner) bool
}

// Outcome of one Run.
type Outcome int

const (
	// Done: the video was saved and task_finish(done) sent.
	Done Outcome = iota
	// Stopped: task_finish was sent for a known end (login required, wrong
	// channel, upload limit) or the server answered control "stop".
	Stopped
	// Failed: Result.Step failed. Result.Fatal means handing just that step
	// to an LLM will not help (unsupported, or an LLM already tried it).
	Failed
)

type Result struct {
	Outcome Outcome
	Step    *Step
	Err     error
	Fatal   bool
	URL     string
}

// Runner walks the steps. It keeps its position and what it has done across
// Run calls, so a run can resume after an LLM fixed a step.
type Runner struct {
	task  Task
	rep   Reporter
	logf  func(string, ...any)
	steps []*Step
	i     int

	videoID, videoURL string
	attached          bool
	short             bool // the dialog linked /shorts/: look in the Shorts tab first
	reported          contract.Status
	handedOff         map[string]bool
	done              []string

	// FailAt makes the named step fail once, before it acts. Only for
	// testing the hand-over.
	FailAt   string
	injected bool

	// PollEvery and StepTimeout bound the waits; UploadTimeout bounds the
	// wait for the upload and processing to finish.
	PollEvery     time.Duration
	StepTimeout   time.Duration
	UploadTimeout time.Duration
	// Settle is the pause after clicks that start an animation (Save) or
	// an upload (thumbnail), and bounds the wait for a Next page change.
	Settle time.Duration
	// StallTimeout ends the upload wait when the progress label has not
	// changed for this long.
	StallTimeout time.Duration
	// StateAfterUpload reads the video's state in Studio after the save and
	// reports it with task_video_state. Off unless the caller sets it (the
	// launcher does, see its -state-after-upload flag).
	StateAfterUpload bool
}

func New(task Task, rep Reporter, logf func(string, ...any)) *Runner {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r := &Runner{
		task: task, rep: rep, logf: logf, handedOff: map[string]bool{},
		PollEvery: time.Second, StepTimeout: 45 * time.Second, UploadTimeout: 30 * time.Minute,
		Settle: 2 * time.Second, StallTimeout: 10 * time.Minute,
	}
	if task.ExistingVideoID != "" {
		r.videoID, r.videoURL, r.attached = task.ExistingVideoID, "https://youtu.be/"+task.ExistingVideoID, true
	}
	r.steps = r.buildSteps()
	return r
}

// Current is the step Run stopped at.
func (r *Runner) Current() *Step {
	if r.i >= len(r.steps) {
		return nil
	}
	return r.steps[r.i]
}

// HandedOff records that an LLM got the current step; if it fails again,
// Run reports it as fatal.
func (r *Runner) HandedOff(step string) { r.handedOff[step] = true }

func (r *Runner) VideoID() string     { return r.videoID }
func (r *Runner) Attached() bool      { return r.attached }
func (r *Runner) DoneSteps() []string { return append([]string(nil), r.done...) }

// terminal is a known end the script finishes itself.
type terminal struct{ code, reason string }

func (t *terminal) Error() string { return t.code + ": " + t.reason }

var errStop = errors.New("server answered control stop")

// fatal marks an error an LLM cannot fix by redoing the step (a stuck
// network, say); the LLM then finishes the task instead.
type fatal struct{ error }

func (f fatal) Unwrap() error { return f.error }

// Run resumes at the current step and goes on until the video is saved, a
// known end is reached, or a step fails.
func (r *Runner) Run(ctx context.Context, p Page) Result {
	if r.reported == "" {
		if err := r.report(ctx, contract.StatusPreparing, "[playbook] start", 0); err != nil {
			return r.end(ctx, p, r.steps[r.i], err)
		}
	}
	if err := r.ensureDialog(ctx, p); err != nil {
		return r.end(ctx, p, r.steps[r.i], err)
	}
	for r.i < len(r.steps) {
		s := r.steps[r.i]
		if s.skip != nil && s.skip(r) {
			r.next(s)
			continue
		}
		if s.Unsupported {
			return r.fail(ctx, p, s, fmt.Errorf("the script does not do %q yet", s.Name), true)
		}
		if s.Check != "" {
			if ok, _ := r.check(ctx, p, s.Check); ok {
				r.logf("playbook: %s already done", s.Name)
				r.next(s)
				continue
			}
		}
		if err := r.report(ctx, s.Status, "[playbook] "+s.Name, 0); err != nil {
			return r.end(ctx, p, s, err)
		}
		began := time.Now()
		r.dismissInterstitials(ctx, p)
		var err error
		if r.FailAt == s.Name && !r.injected {
			r.injected = true
			err = fmt.Errorf("failure injected at %s", s.Name)
		} else {
			err = s.do(ctx, r, p)
			if err == nil && s.Check != "" {
				err = r.waitCheck(ctx, p, s.Check, r.StepTimeout)
				// A notice that popped up during the step can hide the result.
				if err != nil && r.dismissInterstitials(ctx, p) > 0 {
					err = r.waitCheck(ctx, p, s.Check, r.StepTimeout/3)
				}
			}
		}
		if err != nil {
			return r.end(ctx, p, s, err)
		}
		r.logf("playbook: %s done in %s", s.Name, time.Since(began).Round(100*time.Millisecond))
		r.next(s)
	}
	if r.StateAfterUpload && r.videoID != "" {
		// A failed read does not undo the upload; it is only logged.
		if st, err := r.ReadVideoState(ctx, p, r.videoID); err != nil {
			r.logf("playbook: read video state: %v", err)
		} else {
			st.Source = "after_upload"
			r.logf("playbook: video %s is %s, processing=%v, restrictions=%q", st.VideoID, st.Visibility, st.Processing, st.Restrictions)
			if _, err := r.rep.VideoState(ctx, st); err != nil {
				r.logf("playbook: task_video_state: %v", err)
			}
		}
	}
	if err := r.rep.Finish(ctx, "done", "", "", r.videoID); err != nil {
		return Result{Outcome: Failed, Err: err, Fatal: true}
	}
	return Result{Outcome: Done}
}

func (r *Runner) next(s *Step) {
	r.done = append(r.done, s.Name)
	r.i++
}

// end turns a step error into a Result.
func (r *Runner) end(ctx context.Context, p Page, s *Step, err error) Result {
	var t *terminal
	switch {
	case errors.Is(err, errStop):
		return Result{Outcome: Stopped, Step: s, Err: err}
	case errors.As(err, &t):
		r.logf("playbook: %s: %v", s.Name, t)
		if ferr := r.rep.Finish(ctx, "needs_attention", t.code, t.reason, r.videoID); ferr != nil {
			return Result{Outcome: Failed, Step: s, Err: ferr, Fatal: true}
		}
		return Result{Outcome: Stopped, Step: s, Err: err}
	case ctx.Err() != nil:
		return Result{Outcome: Failed, Step: s, Err: ctx.Err(), Fatal: true}
	}
	var f fatal
	if errors.As(err, &f) {
		return r.fail(ctx, p, s, err, true)
	}
	return r.fail(ctx, p, s, err, s.Check == "" || r.handedOff[s.Name])
}

func (r *Runner) fail(ctx context.Context, p Page, s *Step, err error, fatal bool) Result {
	u, _ := p.URL(ctx)
	r.logf("playbook: %s failed (fatal=%v): %v", s.Name, fatal, err)
	return Result{Outcome: Failed, Step: s, Err: err, Fatal: fatal, URL: u}
}

func (r *Runner) report(ctx context.Context, status contract.Status, msg string, progress int) error {
	if status == "" || (status == r.reported && progress == 0) {
		return nil
	}
	r.reported = status
	stop, err := r.rep.Report(ctx, status, msg, progress)
	if err != nil {
		// A lost report is not worth failing the upload for.
		r.logf("playbook: task_report: %v", err)
		return nil
	}
	if stop {
		return errStop
	}
	return nil
}

func (r *Runner) check(ctx context.Context, p Page, expr string) (bool, error) {
	var ok bool
	err := p.Evaluate(ctx, expr, &ok)
	return ok, err
}

func (r *Runner) waitCheck(ctx context.Context, p Page, expr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		ok, err := r.check(ctx, p, expr)
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("check did not hold within %s: %w", d, err)
			}
			return fmt.Errorf("check did not hold within %s", d)
		}
		if err := sleep(ctx, r.PollEvery); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
