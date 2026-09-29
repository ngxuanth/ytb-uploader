package upload

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// Runners.
const (
	RunnerPlaybook = "playbook"     // the script did everything
	RunnerMixed    = "playbook+llm" // the script handed steps to an LLM
	RunnerLLM      = "llm"          // an LLM session did the task
)

// Attempt is one attempt of a task.
type Attempt struct {
	ID          string // task id, used in logs
	SessionDir  string
	Timeout     time.Duration
	Runner      string // RunnerPlaybook (default) or RunnerLLM
	MaxHandoffs int    // step hand-overs before the LLM finishes the task (default 2)
	Logf        func(string, ...any)
}

// Summary is how the attempt went.
type Summary struct {
	Runner      string
	FailedSteps []string
	Handoffs    int
	Out         Outcome // the last LLM session, zero when none ran
	Err         error
}

// Run does the attempt.
func Run(ctx context.Context, a Attempt, tasks Tasks, env Env) Summary {
	start := time.Now()
	deadline := start.Add(a.Timeout)
	remaining := func() time.Duration { return time.Until(deadline) }
	logf := func(f string, args ...any) {
		if a.Logf != nil {
			a.Logf("task %s: "+f, append([]any{a.ID}, args...)...)
		}
	}
	sum := Summary{Runner: RunnerPlaybook}
	if a.MaxHandoffs <= 0 {
		a.MaxHandoffs = 2
	}
	llmOnly := func() Summary {
		sum.Runner = RunnerLLM
		sum.Out, sum.Err = llm(ctx, env, Session{Dir: a.SessionDir, Kind: PromptTask, Timeout: remaining()})
		return sum
	}

	if a.Runner == RunnerLLM {
		return llmOnly()
	}
	claim, err := tasks.Claim(ctx)
	switch {
	case err != nil:
		logf("playbook claim: %v; running the LLM", err)
		return llmOnly()
	case claim.Stop || claim.TaskID == "":
		logf("nothing to do (%s)", claim.Message)
		return sum
	case claim.Kind == KindCheckVideo:
		return check(ctx, tasks, env, claim, sum, logf)
	case claim.Kind != KindUpload || claim.Metadata == nil:
		// Deleting videos is LLM work.
		return llmOnly()
	}

	script := env.Script(claim)
	for first := true; ; first = false {
		res := script.Run(ctx, first)
		if !res.Failed {
			logf("playbook ended after %s", time.Since(start).Round(time.Second))
			return sum
		}
		if ctx.Err() != nil {
			sum.Err = ctx.Err()
			return sum
		}
		stepName := "?"
		if res.Step != nil {
			stepName = res.Step.Name
		}
		if n := len(sum.FailedSteps); n == 0 || sum.FailedSteps[n-1] != stepName {
			sum.FailedSteps = append(sum.FailedSteps, stepName)
		}
		sum.Runner = RunnerMixed
		sc := StepContext{
			TaskID: claim.TaskID, Step: stepName, URL: res.URL, VideoID: script.VideoID(),
			Done: script.Done(), Title: claim.Metadata.Title, Channel: claim.ChannelID,
			Kids: claim.Metadata.MadeForKids, Visible: string(claim.Metadata.Visibility), Snapshot: res.Snapshot,
			Attached: script.Attached(),
		}
		if res.Err != nil {
			sc.Error = res.Err.Error()
		}
		if res.Step != nil {
			sc.Goal = res.Step.Goal
		}

		if res.Fatal || res.Step == nil || res.Step.Check == "" || sum.Handoffs >= a.MaxHandoffs {
			logf("step %s cannot be handed over on its own (%v); the LLM finishes the task", stepName, res.Err)
			script.HideVideo()
			sum.Out, sum.Err = llm(ctx, env, Session{Dir: a.SessionDir, Kind: PromptHandoff, Context: sc, Timeout: remaining()})
			return sum
		}

		sum.Handoffs++
		script.HandedOff(stepName)
		dir := filepath.Join(a.SessionDir, fmt.Sprintf("step-%d-%s", sum.Handoffs, stepName))
		logf("handing step %s to the LLM (%s)", stepName, dir)
		budget := min(remaining(), 10*time.Minute)
		out, reached, err := fixStep(ctx, env, Session{Dir: dir, Kind: PromptStep, Context: sc, Timeout: budget}, res.Step.Check, logf)
		sum.Out = out
		if err != nil {
			logf("step session: %v", err)
		}
		if ctx.Err() != nil {
			// Cancelled or the launcher is stopping: nothing to resume.
			sum.Err = ctx.Err()
			return sum
		}
		logf("step %s session ended (goal reached: %v); the script checks the page and goes on", stepName, reached)
		if remaining() <= 0 {
			sum.Err = errors.New("task deadline passed")
			return sum
		}
	}
}

// check runs a check_video task: the script reads the video's state in
// Studio and reports it. No LLM: when the page cannot be read the task
// fails and can be retried.
func check(ctx context.Context, tasks Tasks, env Env, claim Claim, sum Summary, logf func(string, ...any)) Summary {
	err := env.CheckVideo(ctx, claim)
	status, code, reason := "done", "", ""
	if err != nil {
		status, code, reason = "failed", "STEP_FAILED", "check_video: "+err.Error()
		logf("%s", reason)
	}
	if ferr := tasks.Finish(ctx, claim.TaskID, status, code, reason, claim.ExistingVideoID); ferr != nil && sum.Err == nil {
		sum.Err = ferr
	}
	return sum
}

func llm(ctx context.Context, env Env, s Session) (Outcome, error) {
	if s.Timeout <= 0 {
		return Outcome{}, errors.New("task deadline passed")
	}
	return env.LLM(ctx, s)
}

// fixStep runs an LLM session for one step and stops it as soon as the
// step's check holds on the page, so a model that forgets to end does not
// hold the upload up.
func fixStep(ctx context.Context, env Env, s Session, check string, logf func(string, ...any)) (Outcome, bool, error) {
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	reachedC := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				if env.PageHolds(wctx, check) {
					logf("step goal reached on the page; stopping the LLM session")
					once.Do(func() { close(reachedC) })
					return
				}
			}
		}
	}()
	s.Stop, s.Grace = reachedC, 3*time.Second
	out, err := llm(ctx, env, s)
	select {
	case <-reachedC:
		return out, true, err
	default:
		return out, env.PageHolds(ctx, check), err
	}
}
