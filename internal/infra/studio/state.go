package studio

import (
	"context"
	"fmt"
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

// rowJS reads videoID's row in the content list (see js/content-row.js).
func rowJS(videoID string) string {
	return eval("content-row", map[string]string{
		"videoID":   videoID,
		"editDraft": reSrc(phEditDraft.alts),
	})
}

// editPageJS reads the video's edit page (see js/edit-page.js).
var editPageJS = eval("edit-page", nil)

// Visibility and resolution words come from the phrase registry (phrases.go).
var (
	visUnlisted  = goRE(phVisUnlisted.alts)
	visPrivate   = goRE(phVisPrivate.alts)
	visPublic    = goRE(phVisPublic.alts)
	visScheduled = goRE(phVisScheduled.alts)
	visDraft     = goRE(phVisDraft.alts)
	resDone      = goRE(phResDone.alts)
	resBusy      = goRE(phResBusy.alts)
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
