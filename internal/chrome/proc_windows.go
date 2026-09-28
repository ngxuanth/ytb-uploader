//go:build windows

package chrome

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func processesContaining(substr string) ([]process, error) {
	script := fmt.Sprintf(`Get-CimInstance Win32_Process -Filter "Name='chrome.exe'" | Where-Object { $_.CommandLine -and $_.CommandLine.Contains('%s') } | ForEach-Object { Write-Output ($_.ProcessId.ToString() + [char]9 + $_.CommandLine) }`, strings.ReplaceAll(substr, "'", "''"))
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("liet ke chrome: %w (%s)", err, truncate(string(ee.Stderr), 300))
		}
		return nil, fmt.Errorf("liet ke chrome: %w", err)
	}
	var procs []process
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidText, cmd, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil {
			continue
		}
		procs = append(procs, process{pid: pid, cmd: cmd})
	}
	return procs, nil
}

func stopProcesses(procs []process) error {
	seen := map[int]bool{}
	for _, p := range procs {
		if seen[p.pid] {
			continue
		}
		seen[p.pid] = true
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.pid)).Run()
	}
	return nil
}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // DETACHED_PROCESS
	}
}
