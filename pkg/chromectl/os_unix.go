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
	flag := "--user-data-dir=" + c.UserDataDir
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), "chrom") {
			continue
		}
		isBrowser, match := true, false
		for _, a := range args[1:] {
			if a == flag {
				match = true
			}
			if strings.HasPrefix(a, "--type=") {
				isBrowser = false
			}
		}
		if match && isBrowser {
			out = append(out, pid)
		}
	}
	return out
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
