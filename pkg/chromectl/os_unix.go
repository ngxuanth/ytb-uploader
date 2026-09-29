//go:build unix

package chromectl

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func pathIDBytes(path string) []byte { return []byte(path) }

func linkDir(link, target string) error {
	return os.Symlink(target, link)
}

func chromePlatformArgs() []string {
	// On Wayland a covered window stops painting and the extension hangs.
	// X11 lets Chrome drive its own frames.
	return []string{"--ozone-platform=x11"}
}

func chromeSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func (c *Controller) chromePIDs() []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if isChromeBrowserFor(raw, c.UserDataDir) {
			out = append(out, pid)
		}
	}
	return out
}

// isChromeBrowserFor reports whether a /proc/<pid>/cmdline is the Chrome
// browser process (not a --type= helper) of userDataDir. Chrome rewrites its
// own cmdline into one space-joined string, so the NUL-separated form cannot
// be relied on.
func isChromeBrowserFor(raw []byte, userDataDir string) bool {
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	var exe string
	var flags []string
	if len(args) == 1 {
		exe, rest, _ := strings.Cut(args[0], " --")
		if rest != "" {
			rest = "--" + rest
		}
		flags = splitFlags(rest)
		args = append([]string{exe}, flags...)
	}
	exe, flags = args[0], args[1:]
	if !strings.Contains(filepath.Base(exe), "chrom") {
		return false
	}
	want := "--user-data-dir=" + userDataDir
	match := false
	for _, a := range flags {
		if strings.HasPrefix(a, "--type=") {
			return false
		}
		if a == want {
			match = true
		}
	}
	return match
}

// splitFlags splits a space-joined flag string at every " --", so values
// that contain spaces (a user-data-dir path) stay whole.
func splitFlags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, " --")
	for i := 1; i < len(parts); i++ {
		parts[i] = "--" + parts[i]
	}
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

func stopPIDs(ctx context.Context, pids []int) error {
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		alive := 0
		for _, p := range pids {
			if syscall.Kill(p, 0) == nil {
				alive++
			}
		}
		if alive == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			for _, p := range pids {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
			return sleep(ctx, time.Second)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

func (c *Controller) lock(ctx context.Context) (func(), error) {
	path := filepath.Join(c.UserDataDir, ".uploader.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			f.Close()
			return nil, err
		}
	}
}
