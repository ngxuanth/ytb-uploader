package studio

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// ReadVideoState reads what YouTube Studio shows about videoID: the row of
// the channel's content list (visibility, restrictions, draft, date) and
// the video's edit page (title, file name, processing of each resolution).
// It works in Vietnamese and English Studio.
func (r *Runner) ReadVideoState(ctx context.Context, p Page, videoID string) (contract.VideoState, error) {
	st := contract.VideoState{VideoID: videoID}
	channel, err := resolveChannel(ctx, r, p)
	if err != nil {
		return st, err
	}
	var row struct {
		Found        bool   `json:"found"`
		Visibility   string `json:"visibility"`
		Restrictions string `json:"restrictions"`
		Date         string `json:"date"`
		Draft        bool   `json:"draft"`
	}
	if err := r.inContentList(ctx, p, channel, func() bool {
		_ = p.Evaluate(ctx, rowJS(videoID), &row)
		return row.Found
	}); err != nil {
		return st, fmt.Errorf("video %s is not in the channel's content list", videoID)
	}
	st.VisibilityText, st.Draft, st.Date = row.Visibility, row.Draft, row.Date
	st.Visibility = normalizeVisibility(row.Visibility, row.Draft)
	st.Restrictions = normalizeRestrictions(row.Restrictions)

	if err := r.navigate(ctx, p, "https://studio.youtube.com/video/"+videoID+"/edit", "/video/"+videoID); err != nil {
		return st, err
	}
	var page struct {
		Ready    bool   `json:"ready"`
		Title    string `json:"title"`
		Filename string `json:"filename"`
		Res      []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
		} `json:"res"`
	}
	if err := r.poll(ctx, func() bool {
		_ = p.Evaluate(ctx, editPageJS, &page)
		return page.Ready
	}); err != nil {
		return st, fmt.Errorf("edit page of %s did not load", videoID)
	}
	st.Title, st.Filename = page.Title, page.Filename
	for _, b := range page.Res {
		st.Resolutions = append(st.Resolutions, contract.ResolutionState{Name: b.Name, State: resolutionState(b.Label), Label: b.Label})
	}
	st.Processing = len(st.Resolutions) == 0
	for _, res := range st.Resolutions {
		if res.State == "processing" {
			st.Processing = true
		}
	}
	st.CheckedAt = time.Now()
	return st, nil
}

// Check is a whole check_video task: read the state and report it.
func (r *Runner) Check(ctx context.Context, p Page) error {
	if r.videoID == "" {
		return fmt.Errorf("check_video needs the video id")
	}
	st, err := r.ReadVideoState(ctx, p, r.videoID)
	if err != nil {
		return err
	}
	st.Source = "check"
	_, err = r.rep.VideoState(ctx, st)
	return err
}

func rowJS(videoID string) string {
	return iife(`const a = document.querySelector('a[href*="/video/` + videoID + `/"]');
if (!a) return {found: false};
const row = a.closest('ytcp-video-row') || a.parentElement;
const g = c => (row.querySelector('.tablecell-' + c)?.innerText || '').trim().replace(/\s+/g, ' ');
const draft = [...row.querySelectorAll('ytcp-button, button, a, [role=button]')].some(b => /chỉnh sửa bản nháp|edit draft/i.test((b.innerText || '') + ' ' + (b.getAttribute('aria-label') || '')));
return {found: true, visibility: g('visibility'), restrictions: g('restrictions'), date: g('date'), draft};`)
}

const editPageJS = `(() => {
const box = document.querySelector('#title-textarea #textbox');
// Each badge has a hidden "-hover" twin; only the shown ones count.
const res = [...document.querySelectorAll('#video-resolutions [id^="badge-"]')].filter(b => b.offsetParent !== null && !b.id.endsWith('-hover')).map(b => ({name: b.id.replace('badge-', ''), label: b.getAttribute('aria-label') || ''}));
return {ready: !!box, title: (box?.textContent || '').trim(), filename: (document.querySelector('#original-filename')?.innerText || '').trim(), res};
})()`

var (
	visUnlisted  = regexp.MustCompile(`(?i)không công khai|unlisted`)
	visPrivate   = regexp.MustCompile(`(?i)riêng tư|private`)
	visPublic    = regexp.MustCompile(`(?i)công khai|public`)
	visScheduled = regexp.MustCompile(`(?i)lên lịch|scheduled`)
	visDraft     = regexp.MustCompile(`(?i)bản nháp|draft`)
	resDone      = regexp.MustCompile(`(?i)đã xử lý xong|processed|hoàn tất|complete`)
	resBusy      = regexp.MustCompile(`(?i)đang xử lý|processing`)
)

func normalizeVisibility(text string, draft bool) string {
	switch {
	case draft || visDraft.MatchString(text):
		return "draft"
	case visScheduled.MatchString(text):
		return "scheduled"
	case visUnlisted.MatchString(text): // before public: "không công khai" contains "công khai"
		return "unlisted"
	case visPrivate.MatchString(text):
		return "private"
	case visPublic.MatchString(text):
		return "public"
	}
	return strings.ToLower(text)
}

func normalizeRestrictions(text string) string {
	t := strings.TrimSpace(text)
	switch strings.ToLower(t) {
	case "", "—", "-", "none", "không có":
		return ""
	}
	return t
}

func resolutionState(label string) string {
	switch {
	case resBusy.MatchString(label) && !resDone.MatchString(label):
		return "processing"
	case resDone.MatchString(label):
		return "processed"
	}
	return "unknown"
}

// navigate opens url; a navigation timeout is fine once the tab is there.
func (r *Runner) navigate(ctx context.Context, p Page, url, want string) error {
	if err := p.Navigate(ctx, url); err != nil {
		if now, _ := p.URL(ctx); !strings.Contains(now, want) {
			return err
		}
		r.logf("playbook: navigate %s: %v, but the page is open; going on", url, err)
	}
	return nil
}

// poll calls ok until it holds or StepTimeout passes.
func (r *Runner) poll(ctx context.Context, ok func() bool) error {
	deadline := time.Now().Add(r.StepTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", r.StepTimeout)
		}
		if err := sleep(ctx, r.PollEvery); err != nil {
			return err
		}
	}
	return nil
}
