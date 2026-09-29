// Package localtask is a task_mcp / report_mcp backend for one task in
// this process, for "launcher try": no server, everything is logged.
package localtask

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Backend serves exactly one task to one session.
type Backend struct {
	sessionID, token string
	task             *taskmcp.ClaimOut
	logf             func(string, ...any)

	mu       sync.Mutex
	last     time.Time
	stop     bool
	video    string
	finishIn *taskmcp.FinishIn
}

// NewBackend serves task to the session holding token; logf gets every call.
func NewBackend(sessionID, token string, task *taskmcp.ClaimOut, logf func(string, ...any)) *Backend {
	return &Backend{sessionID: sessionID, token: token, task: task, logf: logf, last: time.Now()}
}

// Token authenticates the session.
func (b *Backend) Token() string { return b.token }

func (b *Backend) touch() taskmcp.Control {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = time.Now()
	if b.stop || b.finishIn != nil {
		return taskmcp.Stop
	}
	return taskmcp.Continue
}

// IdleFor is how long since the session last called a tool.
func (b *Backend) IdleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.last)
}

// RequestStop makes every later call answer control "stop".
func (b *Backend) RequestStop() { b.mu.Lock(); b.stop = true; b.mu.Unlock() }

// VideoID is the id task_video_created reported.
func (b *Backend) VideoID() string { b.mu.Lock(); defer b.mu.Unlock(); return b.video }

// Finished is the task_finish call, nil before it.
func (b *Backend) Finished() *taskmcp.FinishIn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.finishIn
}

func (b *Backend) Authenticate(_ context.Context, token string) (*taskmcp.Session, error) {
	if !taskmcp.TokenEqual(token, b.token) {
		return nil, taskmcp.ErrUnauthorized
	}
	return &taskmcp.Session{ID: b.sessionID}, nil
}

func (b *Backend) Claim(context.Context, *taskmcp.Session) (*taskmcp.ClaimOut, error) {
	c := b.touch()
	b.logf("task_claim -> %s", b.task.TaskID)
	if c == taskmcp.Stop {
		return &taskmcp.ClaimOut{Control: taskmcp.Stop, Message: "task cancelled"}, nil
	}
	out := *b.task
	// An LLM taking over from the script must finish the video it created.
	if v := b.VideoID(); out.ExistingVideoID == "" && v != "" {
		out.ExistingVideoID = v
	}
	return &out, nil
}

func (b *Backend) checkTask(id string) error {
	if id != b.task.TaskID {
		return fmt.Errorf("unknown task_id %q, this session's task is %q", id, b.task.TaskID)
	}
	return nil
}

func (b *Backend) Report(_ context.Context, _ *taskmcp.Session, in taskmcp.ReportIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	c := b.touch()
	b.logf("task_report %-16s %3d%%  %s", in.Step, in.Progress, in.Message)
	return ack(c), nil
}

func (b *Backend) VideoCreated(_ context.Context, _ *taskmcp.Session, in taskmcp.VideoCreatedIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	c := b.touch()
	b.mu.Lock()
	b.video = in.VideoID
	b.mu.Unlock()
	b.logf("task_video_created %s %s", in.VideoID, in.VideoURL)
	return ack(c), nil
}

func (b *Backend) VideoState(_ context.Context, _ *taskmcp.Session, in taskmcp.VideoStateIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	c := b.touch()
	b.logf("task_video_state %s visibility=%s processing=%v restrictions=%q resolutions=%v", in.VideoID, in.Visibility, in.Processing, in.Restrictions, in.Resolutions)
	return ack(c), nil
}

func (b *Backend) Finish(_ context.Context, _ *taskmcp.Session, in taskmcp.FinishIn) (*taskmcp.Ack, error) {
	if err := b.checkTask(in.TaskID); err != nil {
		return nil, err
	}
	b.touch()
	b.mu.Lock()
	if b.finishIn == nil {
		b.finishIn = &in
	}
	b.mu.Unlock()
	b.logf("task_finish %s video=%s code=%s reason=%s", in.Status, in.VideoID, in.ErrorCode, in.Reason)
	return &taskmcp.Ack{Control: taskmcp.Stop, Message: "recorded; end the session now"}, nil
}

func ack(c taskmcp.Control) *taskmcp.Ack {
	a := &taskmcp.Ack{Control: c}
	if c == taskmcp.Stop {
		a.Message = "task cancelled: stop now and end the session"
	}
	return a
}

// VerifyVideo does not trust the LLM: the id must match the one reported
// when the video was created, and public/unlisted videos must be reachable.
func VerifyVideo(created, finished string, vis contract.Visibility) (bool, string) {
	if finished == "" {
		return false, "no video_id in task_finish"
	}
	if created != "" && created != finished {
		return false, fmt.Sprintf("task_finish video %s differs from created video %s", finished, created)
	}
	if vis == contract.VisibilityPrivate {
		return false, "private video: not publicly checkable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape("https://www.youtube.com/watch?v="+finished)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "oembed: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, "oembed ok"
	}
	return false, "oembed " + resp.Status
}
