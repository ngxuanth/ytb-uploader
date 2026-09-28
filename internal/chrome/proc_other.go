//go:build !windows

package chrome

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// processesContaining đọc /proc, lấy mọi process chrome có command line chứa substr.
func processesContaining(substr string) ([]process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// Chrome ghi đè cmdline thành một chuỗi cách nhau bằng dấu cách, nên
		// nhận diện qua exe thay vì argv[0].
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil || filepath.Base(exe) != "chrome" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(strings.TrimRight(string(raw), "\x00"), "\x00", " ")
		if strings.Contains(cmd, substr) {
			procs = append(procs, process{pid: pid, cmd: cmd})
		}
	}
	return procs, nil
}

func stopProcesses(procs []process) error {
	for _, p := range procs {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
	return nil
}

// detach cho Chrome process group riêng để Ctrl+C ở terminal worker không
// kill Chrome giữa lúc agent đang dùng.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
