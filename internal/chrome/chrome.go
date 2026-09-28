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
	"strings"
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
	// Chrome đang chạy đã có extension: nạp lại sẽ reload nó, service worker cũ
	// chết mà service worker mới chưa chắc lên kịp, nên chọn tab luôn.
	if tabID, err := trySelectTab(ctx, cfg.ChromeDebugPort); err == nil {
		log.Info("browsermcp da chon tab", slog.Int("tab_id", tabID), slog.String("profile", profile.Name))
		return nil
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

func runningWanted(procs []process, userData, profileName, extDir string, port int) bool {
	portFlag := fmt.Sprintf("--remote-debugging-port=%d", port)
	for _, p := range procs {
		if hasFlag(p.cmd, "--user-data-dir="+userData) &&
			hasFlag(p.cmd, "--profile-directory="+profileName) &&
			hasFlag(p.cmd, "--load-extension="+extDir) &&
			hasFlag(p.cmd, portFlag) {
			return true
		}
	}
	return false
}

// hasFlag khớp cả giá trị của flag, để profile "kenh1" không khớp "kenh10".
// Sau giá trị là hết chuỗi, dấu cách, hoặc dấu " (Windows quote arg có dấu cách).
func hasFlag(cmd, flag string) bool {
	for rest := cmd; ; {
		i := strings.Index(rest, flag)
		if i < 0 {
			return false
		}
		rest = rest[i+len(flag):]
		if rest == "" || rest[0] == ' ' || rest[0] == '"' {
			return true
		}
	}
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
	detach(cmd)
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
