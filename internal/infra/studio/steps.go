package studio

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

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
