// Package upload is one attempt of a task on one Chrome profile: the
// playbook script first, an LLM session for a step the script cannot do,
// and an LLM for the rest when a hand-over does not help. It sees the task
// server, the browser and the LLM only through the ports below.
package upload

import (
	"context"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Task kinds a claim can carry.
const (
	KindUpload     = "upload"
	KindCheckVideo = "check_video"
)

// Claim is what task_claim handed out.
type Claim struct {
	Stop            bool   // the server answered control "stop"
	Message         string // why, when Stop
	Kind            string
	TaskID          string
	ExistingVideoID string
	ChannelID       string
	FilePath        string // relative to the attempt's upload dir
	ThumbnailPath   string
	Metadata        *contract.Metadata
}

// Tasks is task_mcp / report_mcp as the attempt itself uses them.
type Tasks interface {
	Claim(ctx context.Context) (Claim, error)
	Finish(ctx context.Context, taskID, status, errorCode, reason, videoID string) error
}

// Step is the playbook step a run stopped at.
type Step struct {
	Name  string
	Goal  string // what must hold on the page after it, in words
	Check string // JS that is true once it holds; empty: cannot be handed over alone
}

// Result is how one Script.Run ended.
type Result struct {
	Failed   bool  // false: done, or stopped for a known reason
	Step     *Step // the failed step, nil when unknown
	Err      error
	Fatal    bool   // handing just this step to an LLM will not help
	URL      string // page URL at the failure
	Snapshot string // aria snapshot at the failure, may be empty
}

// Script is the playbook for one upload. It keeps its position across Run
// calls, so a run resumes after an LLM fixed a step.
type Script interface {
	// Run drives the browser from the current step. first is the first run
	// of the attempt (Chrome and the extension still have to be set up).
	Run(ctx context.Context, first bool) Result
	VideoID() string
	Done() []string // steps finished
	Attached() bool // the video file is attached to the upload dialog
	HandedOff(step string)
	// HideVideo moves the attached file out of the LLM's reach, so an LLM
	// finishing the task cannot upload a second copy.
	HideVideo()
}

// Prompt kinds.
type PromptKind int

const (
	PromptTask    PromptKind = iota // do the whole task from task_claim
	PromptStep                      // fix one step, then end
	PromptHandoff                   // finish the task the script started
)

// StepContext is what the LLM is told about the script's state.
type StepContext struct {
	TaskID   string
	Step     string
	Goal     string
	Error    string
	URL      string
	VideoID  string
	Done     []string
	Title    string
	Channel  string
	Kids     bool
	Visible  string
	Snapshot string
	Attached bool
}

// Session is one LLM session.
type Session struct {
	Dir     string // where its files and transcript go
	Kind    PromptKind
	Context StepContext     // for PromptStep and PromptHandoff
	Stop    <-chan struct{} // closed: end it early (goal reached)
	Timeout time.Duration
	Grace   time.Duration // after Stop, before it is killed
}

// Outcome is the ended LLM session.
type Outcome struct {
	ExitCode int
	Signaled bool
	KilledBy string
	NumTurns int
	CostUSD  float64
	Duration time.Duration
}

// Env is the attempt's own Chrome profile, browser driver and LLM sessions.
type Env interface {
	Script(c Claim) Script
	// CheckVideo reads the video's state in Studio and reports it.
	CheckVideo(ctx context.Context, c Claim) error
	LLM(ctx context.Context, s Session) (Outcome, error)
	// PageHolds evaluates a step's Check in the profile's Studio tabs.
	PageHolds(ctx context.Context, check string) bool
}
