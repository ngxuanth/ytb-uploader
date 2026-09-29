package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// fakeStudio imitates the upload dialog. Evaluate answers the playbook's
// expressions by recognising the selectors in them.
type fakeStudio struct {
	meta contract.Metadata
	url  string

	uploads    int
	link       string
	title      string
	desc       string
	audience   bool
	nextClicks int
	visibility string
	saved      bool

	brokenTitle bool // typing the title leaves the old text, like a lost focus
	limitText   string

	dialogOpen bool   // the upload dialog is on screen
	notice     bool   // the "Use of AI" notice covers the dialog
	drafts     string // video id listed as a draft in the content list
	closed     int    // notices closed
	stuck      bool   // the upload never gets past a fixed percent
	domNext    bool   // DOM clicks on Next are ignored (only real clicks work)
	realNext   int    // real (CDP) clicks on Next
	domIgnore  bool   // DOM clicks on radios and Save do nothing
	realClicks int    // real (CDP) clicks on radios and Save

	rowVisibility string           // Visibility column in the content list
	rowMissing    bool             // the video is not in the content list
	short         bool             // the upload is a Short: shorts/ link, listed in the Shorts tab
	navs          []string         // every Navigate
	badges        []map[string]any // #video-resolutions badges on the edit page
}

func newStudio(meta contract.Metadata) *fakeStudio {
	return &fakeStudio{meta: meta, url: "https://studio.youtube.com/channel/UC0123456789abcdef/videos/upload?d=ud", title: "video", dialogOpen: true,
		rowVisibility: "Riêng tư", badges: []map[string]any{{"name": "sd", "label": "Đã xử lý xong độ phân giải chuẩn"}}}
}

func (f *fakeStudio) Navigate(_ context.Context, url string) error {
	f.url = url
	f.navs = append(f.navs, url)
	return nil
}

// visited reports whether a navigation went to a URL ending in suffix.
func (f *fakeStudio) visited(suffix string) bool {
	for _, u := range f.navs {
		if strings.HasSuffix(u, suffix) {
			return true
		}
	}
	return false
}
func (f *fakeStudio) URL(context.Context) (string, error)  { return f.url, nil }
func (f *fakeStudio) Scroll(context.Context, string) error { return nil }

func (f *fakeStudio) UploadFile(_ context.Context, sel, _ string) error {
	if sel == "input[type=file]" {
		f.uploads++
		switch {
		case f.limitText != "":
		case f.short:
			f.link = "https://youtube.com/shorts/abcdefghijk"
		default:
			f.link = "https://youtu.be/abcdefghijk"
		}
	}
	return nil
}

func (f *fakeStudio) TypeSelector(_ context.Context, sel, text string, _ bool) error {
	switch {
	case strings.Contains(sel, "title"):
		if !f.brokenTitle {
			f.title = text
		}
	case strings.Contains(sel, "description"):
		f.desc = text
	}
	return nil
}

func (f *fakeStudio) ClickSelector(_ context.Context, sel string) error {
	if f.notice {
		return errors.New("click timeout: element covered")
	}
	if sel != "#next-button" {
		f.realClicks++
	}
	switch {
	case strings.Contains(sel, "VIDEO_MADE_FOR_KIDS"):
		f.audience = true
	case sel == "#next-button":
		f.nextClicks++
		f.realNext++
	case strings.Contains(sel, `name="PRIVATE"`), strings.Contains(sel, `name="PUBLIC"`), strings.Contains(sel, `name="UNLISTED"`):
		if f.nextClicks >= 3 {
			f.visibility = sel
		}
	case sel == "#done-button":
		f.saved = true
	}
	return nil
}

func (f *fakeStudio) Evaluate(_ context.Context, expr string, out any) error {
	var v any
	switch {
	case strings.Contains(expr, ".tablecell-"):
		if f.rowMissing || (f.short && !strings.Contains(f.url, "/videos/short")) {
			v = map[string]any{"found": false}
		} else {
			v = map[string]any{"found": true, "visibility": f.rowVisibility, "restrictions": "—", "date": "29 thg 9, 2026 Ngày tải lên", "draft": false}
		}
	case strings.Contains(expr, "#video-resolutions"):
		v = map[string]any{"ready": true, "title": f.title, "filename": "video.mp4", "res": f.badges}
	case strings.Contains(expr, "return vis(e) && !e.hasAttribute('disabled')"):
		v = true // the element is still there to click
	case strings.Contains(expr, "e.click(); return true"):
		if !f.domIgnore {
			switch {
			case strings.Contains(expr, "VIDEO_MADE_FOR_KIDS"):
				f.audience = true
			case strings.Contains(expr, `name=\"PRIVATE\"`), strings.Contains(expr, `name=\"PUBLIC\"`), strings.Contains(expr, `name=\"UNLISTED\"`):
				if f.nextClicks >= 3 {
					f.visibility = "dom"
				}
			case strings.Contains(expr, "#done-button"):
				f.saved = true
			}
		}
		v = true
	case strings.Contains(expr, "b.click(); return true"):
		if !f.domNext {
			f.nextClicks++
		}
		v = true
	case strings.Contains(expr, "ytcp-uploads-review"):
		v = fmt.Sprintf("page-%d", min(f.nextClicks, 3))
	case strings.Contains(expr, "sử dụng ai"):
		v = []string{}
		if f.notice {
			f.notice = false
			f.closed++
			v = []string{"Để tuân thủ chính sách của YouTube, hãy chuyển đến mục Sử dụng AI"}
		}
	case strings.Contains(expr, "chỉnh sửa bản nháp"):
		switch {
		case f.drafts == "":
			v = "missing"
		default:
			f.dialogOpen = true
			v = "ok"
		}
	case strings.Contains(expr, "ytcp-uploads-dialog #next-button"):
		v = f.dialogOpen
	case strings.Contains(expr, "limit:"):
		v = map[string]any{"link": f.link, "limit": f.limitText != "" && limitRe(expr).MatchString(strings.ToLower(f.limitText))}
	case strings.Contains(expr, "ytcp-video-upload-progress"):
		v = map[string]any{"done": !f.stuck, "label": "Đang tải lên 37%"}
	case strings.Contains(expr, "ytcp-video-share-dialog"):
		v = f.saved
	case strings.Contains(expr, "ytcp-prechecks-warning-dialog"):
		v = false
	case strings.Contains(expr, "#done-button"):
		v = f.nextClicks >= 3 && !f.stuck
	case strings.Contains(expr, "input[type=file]"):
		v = strings.Contains(f.url, "/videos/upload")
	case strings.Contains(expr, "ytcp-uploads-dialog a[href]") && strings.Contains(expr, "return !!"):
		v = f.link != ""
	case strings.Contains(expr, "ytcp-uploads-dialog a[href]"):
		v = f.link
	case strings.Contains(expr, "#title-textarea"):
		v = strings.TrimSpace(f.title) == strings.TrimSpace(f.meta.Title)
	case strings.Contains(expr, "#description-textarea"):
		v = strings.Contains(f.desc, strings.TrimSpace(f.meta.Description))
	case strings.Contains(expr, "VIDEO_MADE_FOR_KIDS"):
		v = f.audience
	case strings.Contains(expr, "#privacy-radios"):
		v = f.nextClicks >= 3
	case strings.Contains(expr, `name=\"PRIVATE\"`), strings.Contains(expr, `name=\"PUBLIC\"`):
		v = f.visibility != ""
	default:
		return errors.New("unexpected expression: " + expr)
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}

type call struct{ kind, a, b string }

type fakeReporter struct {
	calls  []call
	stop   bool
	states []contract.VideoState
}

func (r *fakeReporter) Report(_ context.Context, step contract.Status, msg string, _ int) (bool, error) {
	r.calls = append(r.calls, call{"report", string(step), msg})
	return r.stop, nil
}

func (r *fakeReporter) VideoCreated(_ context.Context, id, _ string) (bool, error) {
	r.calls = append(r.calls, call{"video", id, ""})
	return false, nil
}

func (r *fakeReporter) Finish(_ context.Context, status, code, _, id string) error {
	r.calls = append(r.calls, call{"finish", status + "/" + code, id})
	return nil
}

func (r *fakeReporter) VideoState(_ context.Context, st contract.VideoState) (bool, error) {
	r.states = append(r.states, st)
	return false, nil
}

func (r *fakeReporter) last() call { return r.calls[len(r.calls)-1] }

func newRunner(meta contract.Metadata, rep Reporter) *Runner {
	r := New(Task{ChannelID: "UC0123456789abcdef", VideoPath: "/tmp/v.mp4", Meta: meta}, rep, nil)
	r.PollEvery, r.StepTimeout, r.Settle = time.Millisecond, 50*time.Millisecond, time.Millisecond
	return r
}

var meta = contract.Metadata{Title: "Đây là video", Description: "Mô tả", Visibility: contract.VisibilityPrivate}

func TestHappyPathUploadsOnceAndFinishes(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	res := newRunner(meta, rep).Run(context.Background(), f)
	if res.Outcome != Done {
		t.Fatalf("result: %+v", res)
	}
	if f.uploads != 1 || !f.saved || f.title != meta.Title || f.visibility == "" {
		t.Fatalf("studio: %+v", f)
	}
	if last := rep.last(); last != (call{"finish", "done/", "abcdefghijk"}) {
		t.Fatalf("finish: %+v", last)
	}
	var steps []string
	for _, c := range rep.calls {
		if c.kind == "report" {
			steps = append(steps, c.a)
		}
	}
	if got := strings.Join(steps, ","); !strings.HasPrefix(got, "PREPARING,ATTACHING,FILLING_METADATA") || !strings.HasSuffix(got, "PUBLISHING") {
		t.Fatalf("reported steps %s", got)
	}
}

func TestLoggedOutFinishesNeedsAttention(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.url = "https://accounts.google.com/signin"
	res := newRunner(meta, rep).Run(context.Background(), f)
	if res.Outcome != Stopped || rep.last() != (call{"finish", "needs_attention/LOGIN_REQUIRED", ""}) {
		t.Fatalf("result %+v, calls %+v", res, rep.calls)
	}
}

func TestResumesAfterTheLLMFixedAStep(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.brokenTitle = true
	r := newRunner(meta, rep)
	res := r.Run(context.Background(), f)
	if res.Outcome != Failed || res.Fatal || res.Step.Name != "title" || r.Current().Name != "title" {
		t.Fatalf("first run: %+v", res)
	}
	if r.VideoID() != "abcdefghijk" || !r.Attached() {
		t.Fatalf("video before hand-over: %q attached=%v", r.VideoID(), r.Attached())
	}
	// The LLM types the title; the script then carries on from there.
	r.HandedOff("title")
	f.title = meta.Title
	res = r.Run(context.Background(), f)
	if res.Outcome != Done || f.uploads != 1 || !f.saved {
		t.Fatalf("resume: %+v uploads=%d", res, f.uploads)
	}
}

func TestStepStillFailingAfterTheLLMIsFatal(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.brokenTitle = true
	r := newRunner(meta, rep)
	_ = r.Run(context.Background(), f)
	r.HandedOff("title")
	res := r.Run(context.Background(), f)
	if res.Outcome != Failed || !res.Fatal || res.Step.Name != "title" || f.uploads != 1 {
		t.Fatalf("second failure: %+v uploads=%d", res, f.uploads)
	}
}

func TestTagsAreHandedToTheLLM(t *testing.T) {
	m := meta
	m.Tags = []string{"a"}
	f, rep := newStudio(m), &fakeReporter{}
	res := newRunner(m, rep).Run(context.Background(), f)
	if res.Outcome != Failed || !res.Fatal || res.Step.Name != "extras" || f.title != m.Title {
		t.Fatalf("extras: %+v", res)
	}
}

func TestInjectedFailureThenResume(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	r := newRunner(meta, rep)
	r.FailAt = "audience"
	res := r.Run(context.Background(), f)
	if res.Outcome != Failed || res.Fatal || res.Step.Name != "audience" || f.audience {
		t.Fatalf("injected: %+v", res)
	}
	r.HandedOff("audience")
	f.audience = true // done by the LLM
	if res := r.Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("resume: %+v", res)
	}
}

func TestServerStopEndsTheRun(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{stop: true}
	res := newRunner(meta, rep).Run(context.Background(), f)
	if res.Outcome != Stopped || f.uploads != 0 {
		t.Fatalf("stop: %+v uploads=%d", res, f.uploads)
	}
}

func TestNoChannelUsesTheProfileDefault(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.url = "https://studio.youtube.com/channel/UCdefault0000000000/videos"
	r := New(Task{VideoPath: "/tmp/v.mp4", Meta: meta}, rep, nil)
	r.PollEvery, r.StepTimeout, r.Settle = time.Millisecond, 50*time.Millisecond, time.Millisecond
	r.StateAfterUpload = false // the test looks at the last URL
	if res := r.Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(f.url, "/channel/UCdefault0000000000/videos/upload") {
		t.Fatalf("url %s", f.url)
	}
}

// limitRe pulls the regular expression out of the attach step's JS, so the
// test checks the page text against exactly what the script looks for.
func limitRe(expr string) *regexp.Regexp {
	i := strings.Index(expr, "limit: /")
	j := strings.Index(expr[i+8:], "/.test")
	return regexp.MustCompile(expr[i+8 : i+8+j])
}

func TestDailyUploadLimitIsNeedsAttention(t *testing.T) {
	for _, text := range []string{
		"Tên tệp video.mp4 Đã đạt giới hạn tải video lên hằng ngày Đăng tải nhiều video hơn mỗi ngày sau khi thực hiện bước xác minh",
		"Daily upload limit reached",
	} {
		f, rep := newStudio(meta), &fakeReporter{}
		f.limitText = text
		res := newRunner(meta, rep).Run(context.Background(), f)
		if res.Outcome != Stopped || rep.last() != (call{"finish", "needs_attention/UPLOAD_LIMIT", ""}) {
			t.Fatalf("%q: result %+v, last %+v", text, res, rep.last())
		}
	}
}

func TestNoticeOverTheDialogIsClosed(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.notice = true
	if res := newRunner(meta, rep).Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("result %+v", res)
	}
	if f.closed != 1 || !f.audience {
		t.Fatalf("closed=%d audience=%v", f.closed, f.audience)
	}
}

func TestExistingVideoReopensItsDraft(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.dialogOpen, f.drafts, f.link = false, "abcdefghijk", "https://youtu.be/abcdefghijk"
	r := New(Task{ExistingVideoID: "abcdefghijk", ChannelID: "UC0123456789abcdef", VideoPath: "/tmp/v.mp4", Meta: meta}, rep, nil)
	r.PollEvery, r.StepTimeout, r.Settle = time.Millisecond, 50*time.Millisecond, time.Millisecond
	r.StateAfterUpload = false // the test looks at the last URL
	res := r.Run(context.Background(), f)
	if res.Outcome != Done || f.uploads != 0 || !f.saved || !f.dialogOpen {
		t.Fatalf("result %+v uploads=%d saved=%v", res, f.uploads, f.saved)
	}
	if !strings.HasSuffix(f.url, "/videos/upload") || rep.last() != (call{"finish", "done/", "abcdefghijk"}) {
		t.Fatalf("url %s last %+v", f.url, rep.last())
	}
}

func TestClosedDialogIsReopenedOnResume(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	r := newRunner(meta, rep)
	r.FailAt = "audience"
	if res := r.Run(context.Background(), f); res.Outcome != Failed || res.Step.Name != "audience" {
		t.Fatalf("first: %+v", res)
	}
	// The LLM closed the upload dialog instead of fixing the step.
	f.dialogOpen, f.drafts = false, "abcdefghijk"
	r.HandedOff("audience")
	if res := r.Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("resume: %+v", res)
	}
	if f.uploads != 1 || !f.dialogOpen || !f.audience || !f.saved {
		t.Fatalf("studio %+v", f)
	}
}

func TestNextUsesDOMClicksAndFallsBackToRealOnes(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	if res := newRunner(meta, rep).Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("dom clicks: %+v", res)
	}
	if f.realNext != 0 || f.nextClicks != 3 {
		t.Fatalf("dom clicks: real=%d next=%d", f.realNext, f.nextClicks)
	}
	f, rep = newStudio(meta), &fakeReporter{}
	f.domNext = true
	if res := newRunner(meta, rep).Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("real clicks: %+v", res)
	}
	if f.realNext != 3 {
		t.Fatalf("real clicks: %d", f.realNext)
	}
}

func TestStalledUploadIsFatal(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.stuck = true
	r := newRunner(meta, rep)
	r.StallTimeout = 20 * time.Millisecond
	res := r.Run(context.Background(), f)
	if res.Outcome != Failed || !res.Fatal || res.Step.Name != "wait_upload" || !strings.Contains(res.Err.Error(), "no progress") {
		t.Fatalf("stall: %+v", res)
	}
}

func TestRadiosAndSaveUseDOMClicksWithRealFallback(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	if res := newRunner(meta, rep).Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("dom: %+v", res)
	}
	if f.realClicks != 0 || !f.audience || f.visibility == "" || !f.saved {
		t.Fatalf("dom: real=%d studio=%+v", f.realClicks, f)
	}
	f, rep = newStudio(meta), &fakeReporter{}
	f.domIgnore = true
	if res := newRunner(meta, rep).Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("fallback: %+v", res)
	}
	if f.realClicks != 3 || !f.audience || !f.saved {
		t.Fatalf("fallback: real=%d studio=%+v", f.realClicks, f)
	}
}

func TestStateNotReadAfterUploadByDefault(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	if res := newRunner(meta, rep).Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("result %+v", res)
	}
	if len(rep.states) != 0 {
		t.Fatalf("state read after upload: %+v", rep.states)
	}
}

func TestStateIsReadAndReportedAfterUpload(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.badges = []map[string]any{{"name": "sd", "label": "Đã xử lý xong độ phân giải chuẩn"}, {"name": "hd", "label": "Đang xử lý độ phân giải cao"}}
	r := newRunner(meta, rep)
	r.StateAfterUpload = true // off by default; see TestStateNotReadAfterUploadByDefault
	if res := r.Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("result %+v", res)
	}
	if len(rep.states) != 1 {
		t.Fatalf("states %+v", rep.states)
	}
	st := rep.states[0]
	if st.VideoID != "abcdefghijk" || st.Visibility != "private" || !st.Processing || st.Restrictions != "" || st.Source != "after_upload" ||
		len(st.Resolutions) != 2 || st.Resolutions[0].State != "processed" || st.Resolutions[1].State != "processing" || st.Title != meta.Title {
		t.Fatalf("state %+v", st)
	}
	if rep.last() != (call{"finish", "done/", "abcdefghijk"}) {
		t.Fatalf("finish after the state: %+v", rep.last())
	}
}

func TestStateReadFailureDoesNotFailTheUpload(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.rowMissing = true
	r := newRunner(meta, rep)
	r.StateAfterUpload = true
	if res := r.Run(context.Background(), f); res.Outcome != Done || len(rep.states) != 0 {
		t.Fatalf("result %+v states %+v", res, rep.states)
	}
}

func TestCheckReportsTheState(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.rowVisibility = "Không công khai"
	r := New(Task{ExistingVideoID: "abcdefghijk", ChannelID: "UC0123456789abcdef"}, rep, nil)
	r.PollEvery, r.StepTimeout, r.Settle = time.Millisecond, 50*time.Millisecond, time.Millisecond
	if err := r.Check(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if len(rep.states) != 1 || rep.states[0].Visibility != "unlisted" || rep.states[0].Processing || rep.states[0].Source != "check" {
		t.Fatalf("states %+v", rep.states)
	}
}

func TestVisibilityWords(t *testing.T) {
	for text, want := range map[string]string{
		"Riêng tư": "private", "Private": "private", "Không công khai": "unlisted", "Unlisted": "unlisted",
		"Công khai": "public", "Public": "public", "Đã lên lịch": "scheduled", "Bản nháp": "draft",
	} {
		if got := normalizeVisibility(text, false); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}

// A vertical video becomes a Short: the dialog links youtube.com/shorts/<id>
// and the content list shows it in the Shorts tab, not the Videos tab.
func TestShortIsUploadedAndItsStateRead(t *testing.T) {
	f, rep := newStudio(meta), &fakeReporter{}
	f.short = true
	r := newRunner(meta, rep)
	r.StateAfterUpload = true
	if res := r.Run(context.Background(), f); res.Outcome != Done {
		t.Fatalf("result %+v", res)
	}
	if f.uploads != 1 || r.VideoID() != "abcdefghijk" {
		t.Fatalf("uploads %d video %q", f.uploads, r.VideoID())
	}
	if len(rep.states) != 1 || rep.states[0].Visibility != "private" {
		t.Fatalf("states %+v", rep.states)
	}
	if !strings.Contains(f.url, "/videos/short") && !strings.Contains(f.url, "/video/") {
		t.Fatalf("last page %s", f.url)
	}
	if f.visited("/videos/upload") {
		t.Fatal("looked in the Videos tab first although the dialog linked a Short")
	}
}
