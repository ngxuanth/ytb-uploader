// Package studio uploads a video through the YouTube Studio upload dialog
// without an LLM. Each step checks the page before and after it acts. When
// a step fails the launcher hands just that step to an LLM session, waits
// until the step's Check holds, then calls Run again: finished steps are
// skipped because their Check already holds, and the script carries on.
package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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

// js quotes s as a JavaScript string literal.
func js(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// vis is a JS helper: the element exists and is laid out on screen.
const vis = `const vis = e => !!e && e.offsetParent !== null && getComputedStyle(e).visibility !== 'hidden';`

func iife(body string) string { return "(() => { " + vis + " " + body + " })()" }

var (
	channelInURL = regexp.MustCompile(`/channel/(UC[\w-]{10,})`)
	// A Short's upload dialog links youtube.com/shorts/<id> instead of youtu.be.
	videoInLink = regexp.MustCompile(`(?:youtu\.be/|/video/|/shorts/)([\w-]{11})`)
	percent     = regexp.MustCompile(`(\d{1,3})\s*%`)
)

func uploadURL(channel string) string {
	return "https://studio.youtube.com/channel/" + channel + "/videos/upload?d=ud"
}

// StartURL is where the launcher opens the tab before the first Run.
func (t Task) StartURL() string {
	if t.ChannelID != "" {
		return uploadURL(t.ChannelID)
	}
	return "https://studio.youtube.com"
}

func radio(name string) string {
	return `tp-yt-paper-radio-button[name="` + name + `"]`
}

func checked(sel string) string {
	return iife(`return document.querySelector(` + js(sel) + `)?.getAttribute('aria-checked') === 'true';`)
}

func (r *Runner) buildSteps() []*Step {
	m := r.task.Meta
	kids := "VIDEO_MADE_FOR_KIDS_NOT_MFK"
	if m.MadeForKids {
		kids = "VIDEO_MADE_FOR_KIDS_MFK"
	}
	visibility := strings.ToUpper(string(m.Visibility))
	if visibility == "" {
		visibility = "PRIVATE"
	}
	const fileInput = `input[type=file]`
	const titleBox = `#title-textarea #textbox`
	const descBox = `#description-textarea #textbox`
	// findLink is a JS expression: the video link shown in the upload
	// dialog, or ''. Studio does not always put it under
	// .video-url-fadeable, so any youtu.be / studio video link in the
	// dialog counts.
	const findLink = `([...document.querySelectorAll('ytcp-uploads-dialog a[href]')].map(a => a.href).find(h => /youtu\.be\/[\w-]{11}|\/video\/[\w-]{11}|\/shorts\/[\w-]{11}/.test(h)) || '')`

	return []*Step{
		{
			Name: "open", Status: contract.StatusPreparing,
			skip:  func(r *Runner) bool { return r.task.ExistingVideoID != "" },
			Goal:  "YouTube Studio upload page of the right channel is open, with the file picker of the upload dialog (input[type=file]) present",
			Check: iife(`return !!document.querySelector(` + js(fileInput) + `) && !location.host.startsWith('accounts.');`),
			do:    doOpen,
		},
		{
			Name: "attach", Status: contract.StatusAttaching,
			skip:  func(r *Runner) bool { return r.task.ExistingVideoID != "" },
			Goal:  "the video file is attached and the upload dialog shows the video link (a https://youtu.be/<id> link, or https://youtube.com/shorts/<id> for a Short, inside ytcp-uploads-dialog)",
			Check: iife(`return !!` + findLink + `;`),
			do: func(ctx context.Context, r *Runner, p Page) error {
				if r.attached {
					// Never attach twice: that would upload a second video.
					return errors.New("file was attached but the video link did not appear")
				}
				if err := p.UploadFile(ctx, fileInput, r.task.VideoPath); err != nil {
					return err
				}
				r.attached = true
				deadline := time.Now().Add(2 * r.StepTimeout)
				for time.Now().Before(deadline) {
					var st struct {
						Link  string `json:"link"`
						Limit bool   `json:"limit"`
					}
					_ = p.Evaluate(ctx, iife(`const t = (document.querySelector('ytcp-uploads-dialog')?.innerText || '').toLowerCase();
return {link: `+findLink+`, limit: /daily upload limit|upload limit reached|giới hạn tải (video )?lên hằng ngày|đã đạt (đến )?giới hạn/.test(t)};`), &st)
					if st.Link != "" {
						return nil
					}
					if st.Limit {
						return &terminal{"UPLOAD_LIMIT", "YouTube Studio says the daily upload limit was reached"}
					}
					if err := sleep(ctx, r.PollEvery); err != nil {
						return err
					}
				}
				return errors.New("the upload dialog did not show the video link")
			},
		},
		{
			// Reads the link and reports it; nothing on the page changes.
			Name: "video_link",
			skip: func(r *Runner) bool { return r.task.ExistingVideoID != "" },
			do: func(ctx context.Context, r *Runner, p Page) error {
				var href string
				if err := p.Evaluate(ctx, iife(`return `+findLink+`;`), &href); err != nil {
					return err
				}
				mm := videoInLink.FindStringSubmatch(href)
				if mm == nil {
					return fmt.Errorf("no video id in link %q", href)
				}
				r.videoID = mm[1]
				r.short = strings.Contains(href, "/shorts/")
				r.videoURL = "https://youtu.be/" + r.videoID
				stop, err := r.rep.VideoCreated(ctx, r.videoID, r.videoURL)
				if err != nil {
					return err
				}
				if stop {
					return errStop
				}
				return nil
			},
		},
		{
			// Only for a video that already exists: reopen its draft.
			Name: "draft", Status: contract.StatusPreparing,
			Goal:  "the upload dialog of the existing video is open again (from the channel's content list, \"Edit draft\")",
			Check: dialogOpenJS,
			skip:  func(r *Runner) bool { return r.task.ExistingVideoID == "" },
			do:    func(ctx context.Context, r *Runner, p Page) error { return r.reopenDraft(ctx, p) },
		},
		{
			Name: "title", Status: contract.StatusFillingMetadata,
			Goal:  "the title textbox (" + titleBox + ") contains exactly " + js(m.Title),
			Check: iife(`return (document.querySelector(` + js(titleBox) + `)?.textContent || '').trim() === ` + js(strings.TrimSpace(m.Title)) + `;`),
			do: func(ctx context.Context, r *Runner, p Page) error {
				return p.TypeSelector(ctx, titleBox, m.Title, false)
			},
		},
		{
			Name:  "description",
			Goal:  "the description textbox (" + descBox + ") contains " + js(m.Description),
			Check: iife(`return (document.querySelector(` + js(descBox) + `)?.textContent || '').includes(` + js(strings.TrimSpace(m.Description)) + `);`),
			skip:  func(r *Runner) bool { return strings.TrimSpace(m.Description) == "" },
			do: func(ctx context.Context, r *Runner, p Page) error {
				return p.TypeSelector(ctx, descBox, m.Description, false)
			},
		},
		{
			// The upload shows no reliable sign of a custom thumbnail, so
			// this step is not checked: a failure goes to a full LLM run.
			Name: "thumbnail",
			Goal: "the custom thumbnail file is uploaded with #file-loader",
			skip: func(r *Runner) bool { return r.task.ThumbPath == "" },
			do: func(ctx context.Context, r *Runner, p Page) error {
				if err := p.UploadFile(ctx, "#file-loader", r.task.ThumbPath); err != nil {
					return err
				}
				return sleep(ctx, 2*r.Settle)
			},
		},
		{
			Name: "extras", Unsupported: true,
			Goal: "playlists and tags are set",
			skip: func(r *Runner) bool { return len(m.Tags) == 0 && len(m.Playlists) == 0 },
		},
		{
			Name:  "audience",
			Goal:  "the audience radio " + radio(kids) + " is selected (aria-checked=\"true\")",
			Check: checked(radio(kids)),
			do: func(ctx context.Context, r *Runner, p Page) error {
				return r.click(ctx, p, radio(kids), checked(radio(kids)), r.Settle)
			},
		},
		{
			Name:  "next",
			Goal:  "the dialog is on its Visibility step (#privacy-radios is visible); press Next (#next-button) until then",
			Check: iife(`return vis(document.querySelector('#privacy-radios'));`),
			do:    doNext,
		},
		{
			Name:        "visibility",
			Goal:        "the visibility radio " + radio(visibility) + " is selected (aria-checked=\"true\")",
			Check:       checked(radio(visibility)),
			Unsupported: m.ScheduleAt != nil,
			do: func(ctx context.Context, r *Runner, p Page) error {
				return r.click(ctx, p, radio(visibility), checked(radio(visibility)), r.Settle)
			},
		},
		{
			Name: "wait_upload", Status: contract.StatusUploading,
			Goal:  "the upload and checks finished and the Save/Publish button (#done-button) is enabled",
			Check: iife(`const b = document.querySelector('#done-button'); return vis(b) && !b.hasAttribute('disabled') && b.getAttribute('aria-disabled') !== 'true';`),
			do:    doWaitUpload,
		},
		{
			Name: "publish", Status: contract.StatusPublishing,
			Goal:  "Save/Publish (#done-button) was pressed and Studio confirmed it: the upload dialog closed, or a confirmation shows (\"Video processing\" / share dialog; close it)",
			Check: publishedJS,
			do: func(ctx context.Context, r *Runner, p Page) error {
				// Studio takes a few seconds to confirm a save.
				if err := r.click(ctx, p, "#done-button", publishedJS, r.StepTimeout/3); err != nil {
					return err
				}
				if ok, _ := r.check(ctx, p, publishedJS); ok {
					r.closeProcessingNotice(ctx, p)
					return nil
				}
				// "Publish anyway" when Studio is still checking the video.
				if err := sleep(ctx, r.Settle); err != nil {
					return err
				}
				var anyway bool
				_ = p.Evaluate(ctx, iife(`const b = [...document.querySelectorAll('ytcp-prechecks-warning-dialog #primary-action-button, ytcp-dialog #secondary-action-button')].find(vis); return !!b;`), &anyway)
				if anyway {
					_ = p.ClickSelector(ctx, "ytcp-prechecks-warning-dialog #primary-action-button")
				}
				return nil
			},
		},
	}
}

// interstitialJS closes notices Studio pops up over the upload dialog, such
// as "to follow YouTube policy, go to 'Use of AI'…". It only clicks a close
// button whose surrounding block has a known notice text and does not hold
// the dialog's own controls, so it can never close the upload dialog.
const interstitialJS = `(() => { ` + vis + `
const notice = /sử dụng ai|use of ai|altered or synthetic|nội dung (bị )?(thay đổi|chỉnh sửa) hoặc (tổng hợp|tạo)/i;
const close = /^(đóng|close|bỏ qua|dismiss|got it|đã hiểu|ok)$/i;
const closed = [];
for (const b of document.querySelectorAll('button, ytcp-button, tp-yt-paper-icon-button, ytcp-icon-button, [role=button]')) {
  if (!vis(b)) continue;
  const label = ((b.innerText || '').trim() || b.getAttribute('aria-label') || '').trim();
  if (!close.test(label)) continue;
  let a = b.parentElement;
  for (let i = 0; i < 8 && a; i++, a = a.parentElement) {
    if (a.querySelector('#title-textarea, #next-button, #privacy-radios, #done-button')) { a = null; break; }
    if (notice.test(a.innerText || '')) break;
  }
  if (a && notice.test(a.innerText || '')) { closed.push((a.innerText || '').trim().slice(0, 100)); b.click(); }
}
return closed; })()`

// dismissInterstitials closes known notices and returns how many it closed.
func (r *Runner) dismissInterstitials(ctx context.Context, p Page) int {
	var closed []string
	if err := p.Evaluate(ctx, interstitialJS, &closed); err != nil || len(closed) == 0 {
		return 0
	}
	for _, c := range closed {
		r.logf("playbook: closed a notice: %q", c)
	}
	_ = sleep(ctx, r.Settle/2)
	return len(closed)
}

const dialogOpenJS = `(() => { ` + vis + ` return vis(document.querySelector('ytcp-uploads-dialog #title-textarea #textbox')) || vis(document.querySelector('#privacy-radios')) || vis(document.querySelector('ytcp-uploads-dialog #next-button')); })()`

// ensureDialog runs before a resumed Run: when the video exists but its
// upload dialog is gone (an LLM closed it, the tab reloaded), the draft is
// reopened and the details steps are checked again from the title on.
func (r *Runner) ensureDialog(ctx context.Context, p Page) error {
	title, publish := r.index("title"), r.index("publish")
	if r.videoID == "" || r.i < title || r.i >= publish {
		return nil
	}
	if ok, _ := r.check(ctx, p, dialogOpenJS); ok {
		return nil
	}
	r.logf("playbook: upload dialog is gone; reopening the draft of %s", r.videoID)
	if err := r.reopenDraft(ctx, p); err != nil {
		return err
	}
	r.i = title
	return nil
}

func (r *Runner) index(name string) int {
	for i, s := range r.steps {
		if s.Name == name {
			return i
		}
	}
	return len(r.steps)
}

// reopenDraft opens the upload dialog of r.videoID again from the channel's
// content list ("Edit draft"), the way a person continues a draft.
func (r *Runner) reopenDraft(ctx context.Context, p Page) error {
	channel, err := resolveChannel(ctx, r, p)
	if err != nil {
		return err
	}
	find := iife(`const a = document.querySelector('a[href*="/video/` + r.videoID + `/"], a[href*="youtu.be/` + r.videoID + `"], a[href*="/shorts/` + r.videoID + `"]');
if (!a) return 'missing';
const row = a.closest('ytcp-video-row, [role=row], tr') || a.parentElement;
const b = [...row.querySelectorAll('ytcp-button, button, a, [role=button]')].find(b => /chỉnh sửa bản nháp|edit draft/i.test((b.innerText || '') + ' ' + (b.getAttribute('aria-label') || '')));
if (!b) return 'not a draft';
b.click(); return 'ok';`)
	var got string
	err = r.inContentList(ctx, p, channel, func() bool {
		_ = p.Evaluate(ctx, find, &got)
		return got == "ok" || got == "not a draft"
	})
	switch {
	case err != nil:
		return fmt.Errorf("video %s is not in the channel's content list", r.videoID)
	case got == "not a draft":
		return fmt.Errorf("video %s is not a draft any more; it needs its edit page", r.videoID)
	}
	if err := r.waitCheck(ctx, p, dialogOpenJS, r.StepTimeout); err != nil {
		return fmt.Errorf("draft %s: dialog did not open: %w", r.videoID, err)
	}
	return nil
}

// contentTabs are the tabs of the channel's content list a new video can be
// in: regular videos, and Shorts (vertical videos up to 3 minutes).
var contentTabs = []string{"upload", "short"}

// inContentList opens each tab of the content list in turn until found
// holds, giving each tab half of StepTimeout.
func (r *Runner) inContentList(ctx context.Context, p Page, channel string, found func() bool) error {
	tabs := contentTabs
	if r.short {
		tabs = []string{"short", "upload"}
	}
	for _, tab := range tabs {
		if err := r.navigate(ctx, p, "https://studio.youtube.com/channel/"+channel+"/videos/"+tab, "/videos"); err != nil {
			return err
		}
		deadline := time.Now().Add(r.StepTimeout / 2)
		for {
			if found() {
				return nil
			}
			if time.Now().After(deadline) {
				break
			}
			if err := sleep(ctx, r.PollEvery); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("not in the content list")
}

// url reads the tab's URL, retrying for a while: a tab the launcher just
// opened has no URL until its first navigation commits.
func (r *Runner) url(ctx context.Context, p Page) (string, error) {
	deadline := time.Now().Add(r.StepTimeout / 3)
	for {
		u, err := p.URL(ctx)
		if err == nil && u != "" {
			return u, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("the tab has no URL")
			}
			return "", err
		}
		if err := sleep(ctx, r.PollEvery); err != nil {
			return "", err
		}
	}
}

// resolveChannel is the task's channel, or the profile's default one read
// from the URL Studio redirects to.
func resolveChannel(ctx context.Context, r *Runner, p Page) (string, error) {
	u, err := r.url(ctx, p)
	if err != nil {
		return "", err
	}
	if strings.Contains(u, "accounts.google.com") {
		return "", &terminal{"LOGIN_REQUIRED", "the profile is logged out of Google"}
	}
	if r.task.ChannelID != "" {
		return r.task.ChannelID, nil
	}
	if mm := channelInURL.FindStringSubmatch(u); mm != nil {
		return mm[1], nil
	}
	if err := p.Navigate(ctx, "https://studio.youtube.com"); err != nil {
		return "", err
	}
	deadline := time.Now().Add(r.StepTimeout)
	for {
		u, _ = p.URL(ctx)
		if strings.Contains(u, "accounts.google.com") {
			return "", &terminal{"LOGIN_REQUIRED", "the profile is logged out of Google"}
		}
		if mm := channelInURL.FindStringSubmatch(u); mm != nil {
			return mm[1], nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("Studio did not open a channel (at %s)", u)
		}
		if err := sleep(ctx, r.PollEvery); err != nil {
			return "", err
		}
	}
}

func doOpen(ctx context.Context, r *Runner, p Page) error {
	u, err := r.url(ctx, p)
	if err != nil {
		return err
	}
	if strings.Contains(u, "accounts.google.com") {
		return &terminal{"LOGIN_REQUIRED", "the profile is logged out of Google"}
	}
	channel := r.task.ChannelID
	if channel == "" {
		// No channel given: Studio redirects to the profile's own channel.
		if !strings.Contains(u, "studio.youtube.com") {
			if err := p.Navigate(ctx, "https://studio.youtube.com"); err != nil {
				return err
			}
		}
		deadline := time.Now().Add(r.StepTimeout)
		for {
			u, _ = p.URL(ctx)
			if strings.Contains(u, "accounts.google.com") {
				return &terminal{"LOGIN_REQUIRED", "the profile is logged out of Google"}
			}
			if mm := channelInURL.FindStringSubmatch(u); mm != nil {
				channel = mm[1]
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("Studio did not open a channel (at %s)", u)
			}
			if err := sleep(ctx, r.PollEvery); err != nil {
				return err
			}
		}
	}
	if !strings.Contains(u, "/channel/"+channel+"/videos/upload") {
		if err := p.Navigate(ctx, uploadURL(channel)); err != nil {
			// Studio can keep a tab "loading" long after the page is
			// usable; the step's check decides whether it really failed.
			if now, _ := p.URL(ctx); !strings.Contains(now, "/videos/upload") {
				return err
			}
			r.logf("playbook: navigate: %v, but the upload page is open; going on", err)
		}
	}
	u, _ = p.URL(ctx)
	if strings.Contains(u, "accounts.google.com") {
		return &terminal{"LOGIN_REQUIRED", "the profile is logged out of Google"}
	}
	if mm := channelInURL.FindStringSubmatch(u); mm != nil && mm[1] != channel {
		return &terminal{"WRONG_CHANNEL", "the profile opened channel " + mm[1] + ", not " + channel}
	}
	return nil
}

func doWaitUpload(ctx context.Context, r *Runner, p Page) error {
	const done = `const b = document.querySelector('#done-button'); return vis(b) && !b.hasAttribute('disabled') && b.getAttribute('aria-disabled') !== 'true';`
	budget := max(r.UploadTimeout, contract.UploadBudget(r.task.VideoSize))
	deadline := time.Now().Add(budget)
	last := -1
	var lastReport time.Time
	lastLabel, lastChange := "", time.Now()
	for {
		var st struct {
			Done  bool   `json:"done"`
			Label string `json:"label"`
		}
		_ = p.Evaluate(ctx, iife(`const l = document.querySelector('ytcp-video-upload-progress'); const d = (() => { `+done+` })();
return {done: d, label: (l?.innerText || '').trim()};`), &st)
		if st.Done {
			return nil
		}
		status, pct := contract.StatusUploading, 0
		if mm := percent.FindStringSubmatch(st.Label); mm != nil {
			pct, _ = strconv.Atoi(mm[1])
		}
		if low := strings.ToLower(st.Label); strings.Contains(low, "xử lý") || strings.Contains(low, "process") || strings.Contains(low, "kiểm tra") || strings.Contains(low, "check") {
			status = contract.StatusProcessing
		}
		if st.Label != lastLabel {
			lastLabel, lastChange = st.Label, time.Now()
		} else if r.StallTimeout > 0 && time.Since(lastChange) > r.StallTimeout {
			return fatal{fmt.Errorf("upload made no progress for %s (%q)", r.StallTimeout, st.Label)}
		}
		if pct != last || time.Since(lastReport) > 20*time.Second {
			last, lastReport = pct, time.Now()
			r.reported = "" // always send progress
			if err := r.report(ctx, status, "[playbook] "+st.Label, pct); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fatal{fmt.Errorf("upload not finished after %s (%s)", budget, st.Label)}
		}
		if err := sleep(ctx, 5*r.PollEvery); err != nil {
			return err
		}
	}
}

// stepSigJS tells which page of the upload dialog is showing, as far as the
// DOM says. It is empty when Studio's markup has none of these; doNext then
// falls back to a fixed pause.
const stepSigJS = `(() => { ` + vis + `
const d = document.querySelector('ytcp-uploads-dialog'); if (!d) return '';
const pages = [...d.querySelectorAll('ytcp-uploads-details, ytcp-uploads-video-elements, ytcp-uploads-checks, ytcp-uploads-review')].filter(vis).map(e => e.tagName.toLowerCase());
const marks = [...d.querySelectorAll('[aria-selected="true"], [aria-current="step"], [active]')].filter(vis).map(e => (e.id || e.tagName.toLowerCase()) + ':' + (e.innerText || '').trim().slice(0, 20));
return pages.concat(marks).join('|'); })()`

const privacyJS = `(() => { ` + vis + ` return vis(document.querySelector('#privacy-radios')); })()`

// nextJS presses Next with a DOM click, which skips the extension's
// animated mouse move (about a second per click).
const nextJS = `(() => { const b = document.querySelector('#next-button');
if (!b || b.hasAttribute('disabled') || b.getAttribute('aria-disabled') === 'true') return false;
b.click(); return true; })()`

// doNext presses Next until the Visibility page shows, waiting for each page
// change instead of a fixed pause.
func doNext(ctx context.Context, r *Runner, p Page) error {
	for range 5 {
		if ok, _ := r.check(ctx, p, privacyJS); ok {
			return nil
		}
		var before string
		_ = p.Evaluate(ctx, stepSigJS, &before)
		r.logf("playbook: next: on page %q", before)
		var pressed bool
		_ = p.Evaluate(ctx, nextJS, &pressed)
		if pressed && r.waitPage(ctx, p, before) {
			continue
		}
		// The DOM click was refused or did nothing: a real click.
		if err := p.ClickSelector(ctx, "#next-button"); err != nil {
			return err
		}
		if !r.waitPage(ctx, p, before) && before == "" {
			// No page signature to watch: give the animation time.
			if err := sleep(ctx, r.Settle); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitPage waits up to 2*Settle for the dialog to leave the page whose
// signature is before, or to reach Visibility.
func (r *Runner) waitPage(ctx context.Context, p Page, before string) bool {
	poll := min(250*time.Millisecond, r.PollEvery)
	deadline := time.Now().Add(2 * r.Settle)
	for time.Now().Before(deadline) {
		if ok, _ := r.check(ctx, p, privacyJS); ok {
			return true
		}
		var now string
		_ = p.Evaluate(ctx, stepSigJS, &now)
		if before != "" && now != "" && now != before {
			return true
		}
		if sleep(ctx, poll) != nil {
			return false
		}
	}
	return false
}

// publishedJS holds once Studio confirmed the save: the share dialog, the
// "video processing" notice, or the upload dialog gone.
var publishedJS = iife(`if (vis(document.querySelector('ytcp-video-share-dialog'))) return true;
if ([...document.querySelectorAll('tp-yt-paper-dialog, ytcp-dialog, [role=dialog]')].some(d => vis(d) && /video processing|still processing|xử lý video|vẫn đang (được )?xử lý|video published|video đã được xuất bản|video đã được lưu/i.test(d.innerText || ''))) return true;
return !vis(document.querySelector('#done-button')) && !vis(document.querySelector('#privacy-radios')) && location.host === 'studio.youtube.com';`)

// closeProcessingNotice closes the "Video processing" confirmation Studio
// shows after saving a video that is still being processed; the save is
// done, the notice only covers the page.
func (r *Runner) closeProcessingNotice(ctx context.Context, p Page) {
	var closed any
	_ = p.Evaluate(ctx, iife(`const d = [...document.querySelectorAll('tp-yt-paper-dialog, ytcp-dialog, [role=dialog]')].find(d => vis(d) && /video processing|still processing|xử lý video|vẫn đang (được )?xử lý/i.test(d.innerText || ''));
if (!d) return false;
const btn = [...d.querySelectorAll('ytcp-button, button, [role=button]')].find(x => vis(x) && /^(đóng|close|ok|đã hiểu|got it)$/i.test(((x.innerText || '').trim()) || x.getAttribute('aria-label') || ''));
if (!btn) return false; btn.click(); return 'closed';`), &closed)
	if closed == "closed" {
		r.logf("playbook: closed the \"video processing\" notice")
	}
}

// clickJS is a DOM click on the element, centred first: no animated mouse
// move, which makes a real click through the extension take seconds.
func clickJS(sel string) string {
	return iife(`const e = document.querySelector(` + js(sel) + `); if (!e) return false; e.scrollIntoView({block: 'center'}); e.click(); return true;`)
}

// click presses sel with a DOM click and checks the result; if the page did
// not react (Studio can ignore synthetic clicks), it does a real click.
func (r *Runner) click(ctx context.Context, p Page, sel, check string, wait time.Duration) error {
	var ok bool
	if err := p.Evaluate(ctx, clickJS(sel), &ok); err == nil && ok {
		if check == "" || r.waitCheck(ctx, p, check, wait) == nil {
			return nil
		}
		// Only click for real when the element is still there to click:
		// once Studio covers it with a confirmation, the click happened.
		var still bool
		_ = p.Evaluate(ctx, iife(`const e = document.querySelector(`+js(sel)+`); return vis(e) && !e.hasAttribute('disabled') && e.getAttribute('aria-disabled') !== 'true';`), &still)
		if !still {
			return nil
		}
		r.logf("playbook: DOM click on %s had no effect; clicking for real", sel)
	}
	if err := p.Scroll(ctx, sel); err != nil {
		return err
	}
	return p.ClickSelector(ctx, sel)
}
