package upload

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

type fakeTasks struct {
	claim    Claim
	err      error
	finished []string
}

func (f *fakeTasks) Claim(context.Context) (Claim, error) { return f.claim, f.err }
func (f *fakeTasks) Finish(_ context.Context, _, status, _, _, _ string) error {
	f.finished = append(f.finished, status)
	return nil
}

// fakeScript fails the steps in fails, in order, then ends.
type fakeScript struct {
	fails    []Result
	runs     int
	handed   []string
	hidden   bool
	attached bool
}

func (s *fakeScript) Run(context.Context, bool) Result {
	s.runs++
	if len(s.fails) == 0 {
		return Result{}
	}
	r := s.fails[0]
	s.fails = s.fails[1:]
	return r
}
func (s *fakeScript) VideoID() string       { return "vid" }
func (s *fakeScript) Done() []string        { return nil }
func (s *fakeScript) Attached() bool        { return s.attached }
func (s *fakeScript) HandedOff(step string) { s.handed = append(s.handed, step) }
func (s *fakeScript) HideVideo()            { s.hidden = true }

type fakeEnv struct {
	script   *fakeScript
	sessions []Session
	holds    bool
	checkErr error
}

func (e *fakeEnv) Script(Claim) Script                     { return e.script }
func (e *fakeEnv) CheckVideo(context.Context, Claim) error { return e.checkErr }
func (e *fakeEnv) PageHolds(context.Context, string) bool  { return e.holds }
func (e *fakeEnv) LLM(_ context.Context, s Session) (Outcome, error) {
	e.sessions = append(e.sessions, s)
	return Outcome{ExitCode: 0}, nil
}

func uploadClaim() Claim {
	return Claim{Kind: KindUpload, TaskID: "t", Metadata: &contract.Metadata{Title: "x"}}
}

func TestScriptOnlyNeedsNoLLM(t *testing.T) {
	env := &fakeEnv{script: &fakeScript{}}
	sum := Run(t.Context(), Attempt{Timeout: time.Minute}, &fakeTasks{claim: uploadClaim()}, env)
	if sum.Runner != RunnerPlaybook || len(env.sessions) != 0 || sum.Err != nil {
		t.Fatalf("%+v %+v", sum, env.sessions)
	}
}

func TestFailedStepIsHandedOverThenScriptGoesOn(t *testing.T) {
	step := &Step{Name: "audience", Check: "true"}
	env := &fakeEnv{script: &fakeScript{fails: []Result{{Failed: true, Step: step, Err: errors.New("no radio")}}}}
	sum := Run(t.Context(), Attempt{Timeout: time.Minute, SessionDir: "/s"}, &fakeTasks{claim: uploadClaim()}, env)
	if sum.Runner != RunnerMixed || sum.Handoffs != 1 || len(sum.FailedSteps) != 1 || sum.FailedSteps[0] != "audience" {
		t.Fatalf("summary %+v", sum)
	}
	if len(env.sessions) != 1 || env.sessions[0].Kind != PromptStep || env.sessions[0].Context.Error != "no radio" || env.sessions[0].Stop == nil {
		t.Fatalf("sessions %+v", env.sessions)
	}
	if env.script.runs != 2 || len(env.script.handed) != 1 {
		t.Fatalf("script %+v", env.script)
	}
}

func TestFatalStepLetsTheLLMFinish(t *testing.T) {
	env := &fakeEnv{script: &fakeScript{attached: true, fails: []Result{{Failed: true, Step: &Step{Name: "open"}, Fatal: true}}}}
	sum := Run(t.Context(), Attempt{Timeout: time.Minute}, &fakeTasks{claim: uploadClaim()}, env)
	if len(env.sessions) != 1 || env.sessions[0].Kind != PromptHandoff || !env.script.hidden || sum.Handoffs != 0 {
		t.Fatalf("%+v %+v", sum, env.sessions)
	}
}

func TestHandoffsAreBounded(t *testing.T) {
	step := &Step{Name: "next", Check: "true"}
	f := Result{Failed: true, Step: step}
	env := &fakeEnv{script: &fakeScript{fails: []Result{f, f, f, f}}}
	sum := Run(t.Context(), Attempt{Timeout: time.Minute}, &fakeTasks{claim: uploadClaim()}, env)
	if sum.Handoffs != 2 || len(env.sessions) != 3 || env.sessions[2].Kind != PromptHandoff {
		t.Fatalf("%+v %+v", sum, env.sessions)
	}
}

func TestClaimErrorFallsBackToLLM(t *testing.T) {
	env := &fakeEnv{}
	sum := Run(t.Context(), Attempt{Timeout: time.Minute}, &fakeTasks{err: errors.New("down")}, env)
	if sum.Runner != RunnerLLM || len(env.sessions) != 1 || env.sessions[0].Kind != PromptTask {
		t.Fatalf("%+v %+v", sum, env.sessions)
	}
}

func TestCheckVideoFinishes(t *testing.T) {
	tasks := &fakeTasks{claim: Claim{Kind: KindCheckVideo, TaskID: "c", ExistingVideoID: "v"}}
	env := &fakeEnv{checkErr: errors.New("no row")}
	Run(t.Context(), Attempt{Timeout: time.Minute}, tasks, env)
	if len(tasks.finished) != 1 || tasks.finished[0] != "failed" || len(env.sessions) != 0 {
		t.Fatalf("%+v", tasks.finished)
	}
}
