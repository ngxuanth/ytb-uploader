package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// fakeStudio imitates the upload dialog. Evaluate answers the playbook's
// expressions by recognising the selectors in them.
type fakeStudio struct {
	meta wire.Metadata
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
}

func newStudio(meta wire.Metadata) *fakeStudio {
	return &fakeStudio{meta: meta, url: "https://studio.youtube.com/channel/UC0123456789abcdef/videos/upload?d=ud", title: "video"}
}

func (f *fakeStudio) Navigate(_ context.Context, url string) error { f.url = url; return nil }
func (f *fakeStudio) URL(context.Context) (string, error)          { return f.url, nil }
func (f *fakeStudio) Scroll(context.Context, string) error         { return nil }

func (f *fakeStudio) UploadFile(_ context.Context, sel, _ string) error {
	if sel == "input[type=file]" {
		f.uploads++
		if f.limitText == "" {
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
	switch {
	case strings.Contains(sel, "VIDEO_MADE_FOR_KIDS"):
		f.audience = true
	case sel == "#next-button":
		f.nextClicks++
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
	case strings.Contains(expr, "limit:"):
		v = map[string]any{"link": f.link, "limit": f.limitText != "" && limitRe(expr).MatchString(strings.ToLower(f.limitText))}
	case strings.Contains(expr, "ytcp-video-upload-progress"):
		v = map[string]any{"done": true, "label": "Đã tải lên 100%"}
	case strings.Contains(expr, "ytcp-video-share-dialog"):
		v = f.saved
	case strings.Contains(expr, "ytcp-prechecks-warning-dialog"):
		v = false
	case strings.Contains(expr, "#done-button"):
		v = f.nextClicks >= 3
	case strings.Contains(expr, "input[type=file]"):
		v = strings.Contains(f.url, "/videos/upload")
	case strings.Contains(expr, ".video-url-fadeable a") && strings.Contains(expr, "|| ''"):
		v = f.link
	case strings.Contains(expr, ".video-url-fadeable a"):
		v = f.link != ""
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
	calls []call
	stop  bool
}

func (r *fakeReporter) Report(_ context.Context, step wire.Status, msg string, _ int) (bool, error) {
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

func (r *fakeReporter) last() call { return r.calls[len(r.calls)-1] }

func newRunner(meta wire.Metadata, rep Reporter) *Runner {
	r := New(Task{ChannelID: "UC0123456789abcdef", VideoPath: "/tmp/v.mp4", Meta: meta}, rep, nil)
	r.PollEvery, r.StepTimeout, r.Settle = time.Millisecond, 50*time.Millisecond, time.Millisecond
	return r
}

var meta = wire.Metadata{Title: "Đây là video", Description: "Mô tả", Visibility: wire.VisibilityPrivate}

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
