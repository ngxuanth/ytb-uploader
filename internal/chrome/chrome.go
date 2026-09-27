// Package chrome mở đúng profile trong chrome-profile và bắt Browser MCP nối tab,
// trước khi harness chạy. Không đụng Chrome hàng ngày của user.
package chrome

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"yt-uploader/internal/config"
)

const youtubeURL = "https://www.youtube.com/"

// Ensure bảo đảm Chrome của project đang chạy đúng profile, đã load extension,
// và extension đã có selectedTabId (tương đương bấm Connect).
func Ensure(ctx context.Context, cfg config.Config, profile config.Profile, log *slog.Logger) error {
	if profile.Dir == "" {
		return fmt.Errorf("profile %s khong co thu muc", profile.Name)
	}
	extDir, err := filepath.Abs(cfg.BrowserMCPExtension)
	if err != nil {
		return err
	}
	if st, err := os.Stat(filepath.Join(extDir, "manifest.json")); err != nil || st.IsDir() {
		return fmt.Errorf("khong thay browsermcp extension tai %s", extDir)
	}
	userData, err := filepath.Abs(filepath.Dir(profile.Dir))
	if err != nil {
		return err
	}

	procs, err := processesContaining(userData)
	if err != nil {
		return err
	}
	if runningWanted(procs, userData, profile.Name, extDir, cfg.ChromeDebugPort) {
		log.Info("chrome da mo dung profile",
			slog.String("profile", profile.Name),
			slog.String("user_data_dir", userData),
		)
	} else {
		if err := stopProcesses(procs); err != nil {
			return err
		}
		if err := waitGone(ctx, userData); err != nil {
			return err
		}
		log.Info("mo chrome",
			slog.String("profile", profile.Name),
			slog.String("user_data_dir", userData),
			slog.String("extension", extDir),
			slog.Int("debug_port", cfg.ChromeDebugPort),
		)
		if err := startChrome(cfg.ChromeBin, userData, profile.Name, extDir, cfg.ChromeDebugPort); err != nil {
			return err
		}
	}

	if err := waitDebug(ctx, cfg.ChromeDebugPort); err != nil {
		return err
	}
	// Chrome branded (từ bản 137, và hẳn từ 142) bỏ qua --load-extension.
	// Nạp unpacked qua CDP sau khi debug port đã mở.
	if err := loadUnpacked(ctx, cfg.ChromeDebugPort, extDir); err != nil {
		return err
	}
	log.Info("da nap browsermcp", slog.String("extension", extDir), slog.String("profile", profile.Name))
	tabID, err := selectBrowserMCPTab(ctx, cfg.ChromeDebugPort)
	if err != nil {
		return err
	}
	log.Info("browsermcp da chon tab", slog.Int("tab_id", tabID), slog.String("profile", profile.Name))
	return nil
}

type process struct {
	pid int
	cmd string
}

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

func runningWanted(procs []process, userData, profileName, extDir string, port int) bool {
	portFlag := fmt.Sprintf("--remote-debugging-port=%d", port)
	for _, p := range procs {
		if strings.Contains(p.cmd, userData) &&
			strings.Contains(p.cmd, profileName) &&
			strings.Contains(p.cmd, extDir) &&
			strings.Contains(p.cmd, portFlag) {
			return true
		}
	}
	return false
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

func waitGone(ctx context.Context, userData string) error {
	deadline := time.Now().Add(8 * time.Second)
	for {
		procs, err := processesContaining(userData)
		if err != nil {
			return err
		}
		if len(procs) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("chrome cu chua tat het")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func startChrome(bin, userData, profileName, extDir string, port int) error {
	args := []string{
		"--user-data-dir=" + userData,
		"--profile-directory=" + profileName,
		"--load-extension=" + extDir,
		"--disable-features=DisableLoadExtensionCommandLineSwitch",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
		"--no-first-run",
		"--no-default-browser-check",
		youtubeURL,
	}
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // DETACHED_PROCESS
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("khong mo duoc chrome: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
