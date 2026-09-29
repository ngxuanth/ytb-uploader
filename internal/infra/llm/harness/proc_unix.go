//go:build unix

package harness

import (
	"syscall"
	"time"
)

func sessionSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func adoptSession(*Process) error { return nil }

func noteExit(p *Process) {
	if p.cmd.ProcessState == nil {
		return
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		p.res.Signaled = true
	}
}

func finishSession(p *Process) {
	if p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

func terminateSession(p *Process, grace time.Duration) {
	if p.cmd.Process == nil {
		return
	}
	pgid := -p.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-p.done
	}
}
