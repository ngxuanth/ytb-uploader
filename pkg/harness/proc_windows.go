//go:build windows

package harness

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func sessionSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func adoptSession(p *Process) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("harness: process not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("harness: job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("harness: job limits: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("harness: open process: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("harness: assign job: %w", err)
	}
	p.sys = uintptr(job)
	return nil
}

func noteExit(p *Process) {
	p.mu.Lock()
	killed := p.killedBy != ""
	p.mu.Unlock()
	if killed {
		p.res.Signaled = true
	}
}

func finishSession(p *Process) {
	if p.sys == 0 {
		return
	}
	job := windows.Handle(p.sys)
	_ = windows.TerminateJobObject(job, 1)
	windows.CloseHandle(job)
	p.sys = 0
}

func terminateSession(p *Process, grace time.Duration) {
	if p.sys != 0 {
		_ = windows.TerminateJobObject(windows.Handle(p.sys), 1)
	} else if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(grace):
		if p.sys != 0 {
			_ = windows.TerminateJobObject(windows.Handle(p.sys), 1)
		}
		<-p.done
	}
}
