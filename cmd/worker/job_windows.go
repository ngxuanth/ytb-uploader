//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ensureKillTree gắn worker vào một job object. Khi process thoát, Windows
// kill mọi process con tạo sau đó (Hermes, Chrome của project).
func ensureKillTree() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return err
	}
	// Giữ handle đến khi process thoát. Đóng sớm sẽ kill chính worker.
	jobKeepalive = job
	return nil
}

var jobKeepalive windows.Handle
