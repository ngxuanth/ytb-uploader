package prompt

import (
	"fmt"
	"strings"
)

// StepContext is what a scripted run tells the LLM when it hands over.
type StepContext struct {
	TaskID    string
	Step      string   // the step that failed
	Goal      string   // what must hold on the page after it
	Error     string   // why the script failed
	URL       string   // page URL when it failed
	VideoID   string   // set once the upload created the video
	Done      []string // steps the script finished
	Title     string
	Channel   string
	Kids      bool
	Visible   string
	Snapshot  string // aria snapshot at the failure, may be empty
	Attached  bool   // the video file was already attached
	Remaining string // for Handoff: what is left to do
}

// Step is the prompt for an LLM that fixes ONE step of a scripted upload.
// The launcher watches the page and stops the session as soon as the step's
// goal holds, then the script carries on.
func Step(c StepContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are fixing ONE step of a YouTube Studio upload that a script is doing for task %s.
A script drives the browser; it stopped at step %q and needs you to finish just that step.

What must be true when you are done (the goal):
  %s

Why the script stopped: %s
Page URL: %s
Steps the script already finished: %s
`, c.TaskID, c.Step, c.Goal, c.Error, c.URL, strings.Join(c.Done, ", "))
	if c.VideoID != "" {
		fmt.Fprintf(&b, "The video already exists: %s. Never attach or upload the file again.\n", c.VideoID)
	}
	fmt.Fprintf(&b, `Task: title %q, made_for_kids=%v, visibility=%s, channel %s.

Rules
1. Chrome is open and the "bmcp" browser tools are already connected to the right tab. Do NOT call chrome_open or extension_setup, do not open other tabs, do not navigate away, do not close the upload dialog.
2. Do only what the goal needs. Do not press Save/Publish unless the goal is about it. Do not call task_claim or task_finish, except task_finish with status "needs_attention" if the page shows a Google login, a captcha or an upload limit.
3. Use browser_snapshot for refs, then browser_scroll (with that ref) before browser_click so sticky bars cannot cover the element. Check your result with browser_evaluate; its expression must be an expression, e.g. (() => { ... })(), not a function.
4. As soon as the goal holds, stop calling tools and end: the script checks the page itself and carries on. You may be stopped automatically once it sees the goal.
`, c.Title, c.Kids, c.Visible, orDefault(c.Channel, "(the profile's default)"))
	if c.Snapshot != "" {
		fmt.Fprintf(&b, "\nPage snapshot when the script stopped (refs are stale; take a new snapshot before clicking):\n%s\n", truncate(c.Snapshot, 6000))
	}
	return b.String()
}

// Handoff is the prompt for an LLM that finishes a task the script could not.
// It is the normal upload prompt plus what the script already did.
func Handoff(c StepContext) string {
	var b strings.Builder
	b.WriteString(Upload)
	fmt.Fprintf(&b, `

Hand-over from the script
A script started this task and stopped at step %q: %s (page %s).
Steps it finished: %s.
`, c.Step, c.Error, c.URL, strings.Join(c.Done, ", "))
	if c.VideoID != "" {
		fmt.Fprintf(&b, `The video was already created: %s. task_claim returns it as existing_video_id: follow rule 4 (finish that video on https://studio.youtube.com/video/%s/edit), never upload the file again. task_video_created was already called.
`, c.VideoID, c.VideoID)
	} else if c.Attached {
		b.WriteString("The file was already attached once; check the channel's videos for a new draft before uploading again, and never upload twice.\n")
	}
	if c.Remaining != "" {
		fmt.Fprintf(&b, "Still to do: %s\n", c.Remaining)
	}
	b.WriteString("Chrome may already be open with the extension connected: call extension_status first and only call chrome_open / extension_setup if it is not connected.\n")
	return b.String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}
