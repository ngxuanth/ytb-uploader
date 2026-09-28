// Package harness runs one LLM CLI session (Claude Code, Cursor Agent, Codex,
// Hermes, …) in its own process group, so killing it also kills the MCP
// servers it spawned. Which CLI, and how it is given its prompt and MCP
// servers, is configuration (see preset.go); nothing here is tied to one
// agent. Task progress does not depend on the CLI's output format: the LLM
// reports through the uploader MCP tools.
package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Command is a fully resolved invocation.
type Command struct {
	Bin    string
	Args   []string
	Env    []string // extra KEY=VALUE on top of the launcher's environment
	Dir    string
	Stdin  string // prompt, when the CLI reads it from stdin
	Parser string // output parser, see parsers
}

// Result summarises a finished session. Stats are best effort: they are
// only filled when the CLI's output format is known.
type Result struct {
	ExitCode int
	Signaled bool // killed by us or by a signal
	NumTurns int
	CostUSD  float64
	IsError  bool
	Text     string // final assistant text, if the format exposes it
	Duration time.Duration
}

// Process is a running session.
type Process struct {
	cmd     *exec.Cmd
	started time.Time
	done    chan struct{}
	res     Result
	waitErr error

	mu       sync.Mutex
	killedBy string
	sys      uintptr // job object on Windows; unused on Linux
}

// LineFunc receives each JSON line of the CLI's stdout as it arrives.
type LineFunc func(ev map[string]any)

// Start launches the session. Raw stdout is written to transcriptPath and
// stderr next to it with a .stderr suffix.
func Start(c Command, transcriptPath string, onLine LineFunc) (*Process, error) {
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		return nil, err
	}
	out, err := os.Create(transcriptPath)
	if err != nil {
		return nil, err
	}
	errf, err := os.Create(transcriptPath + ".stderr")
	if err != nil {
		out.Close()
		return nil, err
	}

	cmd := exec.Command(c.Bin, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.SysProcAttr = sessionSysProcAttr()
	cmd.Stderr = errf
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		out.Close()
		errf.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		out.Close()
		errf.Close()
		return nil, fmt.Errorf("harness: start %s: %w", c.Bin, err)
	}

	parse := parsers[c.Parser]
	p := &Process{cmd: cmd, started: time.Now(), done: make(chan struct{})}
	if err := adoptSession(p); err != nil {
		_ = cmd.Process.Kill()
		out.Close()
		errf.Close()
		return nil, err
	}
	go func() {
		defer close(p.done)
		p.consume(io.TeeReader(stdout, out), parse, onLine)
		p.waitErr = cmd.Wait()
		out.Close()
		errf.Close()
		p.res.Duration = time.Since(p.started)
		if cmd.ProcessState != nil {
			p.res.ExitCode = cmd.ProcessState.ExitCode()
		}
		noteExit(p)
		// The CLI is gone; make sure nothing it spawned outlives it.
		finishSession(p)
	}()
	return p, nil
}

func (p *Process) consume(r io.Reader, parse func(map[string]any, *Result), onLine LineFunc) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if parse != nil {
			parse(ev, &p.res)
		}
		if onLine != nil {
			onLine(ev)
		}
	}
}

// parsers pull session stats out of known JSON-lines output formats.
var parsers = map[string]func(map[string]any, *Result){
	// Claude Code and Cursor Agent: --output-format stream-json ends with
	// {"type":"result", ...}.
	"stream-json": func(ev map[string]any, r *Result) {
		if ev["type"] != "result" {
			return
		}
		if n, ok := ev["num_turns"].(float64); ok {
			r.NumTurns = int(n)
		}
		if c, ok := ev["total_cost_usd"].(float64); ok {
			r.CostUSD = c
		}
		if e, ok := ev["is_error"].(bool); ok {
			r.IsError = e
		}
		if t, ok := ev["result"].(string); ok {
			r.Text = t
		}
	},
	// Hermes: `hermes chat --format stream-json` emits tool_use/tool_result
	// events and ends with {"type":"result","exit_code":N,"text":...}.
	"hermes-json": func(ev map[string]any, r *Result) {
		switch ev["type"] {
		case "tool_use":
			r.NumTurns++
		case "result":
			if c, ok := ev["exit_code"].(float64); ok && c != 0 {
				r.IsError = true
			}
			if _, ok := ev["error"]; ok {
				r.IsError = true
			}
			if t, ok := ev["text"].(string); ok {
				r.Text = t
			}
		}
	},
	// Codex: `codex exec --json` emits one event per line; count turns.
	"codex-json": func(ev map[string]any, r *Result) {
		if ev["type"] == "turn.completed" {
			r.NumTurns++
		}
		if ev["type"] == "turn.failed" {
			r.IsError = true
		}
	},
}

func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done is closed when the session has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until exit or ctx ends (ctx ending does not kill).
func (p *Process) Wait(ctx context.Context) (Result, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.killedBy != "" {
			p.res.Signaled = true
		}
		var ee *exec.ExitError
		if p.waitErr != nil && !errors.As(p.waitErr, &ee) {
			return p.res, p.waitErr
		}
		return p.res, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Kill stops the session and everything it spawned.
// Linux signals the process group. Windows terminates the job object.
func (p *Process) Kill(reason string, grace time.Duration) {
	p.mu.Lock()
	if p.killedBy == "" {
		p.killedBy = reason
	}
	p.mu.Unlock()
	terminateSession(p, grace)
}

// KilledBy is the reason given to the first Kill call, if any.
func (p *Process) KilledBy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killedBy
}
