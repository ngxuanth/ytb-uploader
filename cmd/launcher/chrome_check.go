package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/chromectl"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/driver"
)

// chromeCheck runs what the LLM's setup phase does (chrome_open,
// extension_setup, extension_status) without an LLM, and listens on the
// session port itself to prove the extension really connects there.
func chromeCheck(args []string) error {
	fs := flag.NewFlagSet("chrome-check", flag.ExitOnError)
	profile := fs.String("profile", "", "Chrome profile directory (required)")
	userDataDir := fs.String("user-data-dir", defaultUserDataDir(), "Chrome user-data-dir")
	debugPort := fs.Int("debug-port", 9222, "Chrome remote debugging port")
	extDir := fs.String("extension", defaultExtensionDir(), "unpacked extension directory")
	chromeBin := fs.String("chrome-bin", "google-chrome", "Chrome binary")
	port := fs.Int("port", 9019, "session port to point the extension at")
	_ = fs.Parse(args)
	if *profile == "" {
		fs.Usage()
		return errors.New("-profile is required")
	}
	udd, _ := filepath.Abs(*userDataDir)
	ext, _ := filepath.Abs(*extDir)
	c := &chromectl.Controller{Bin: *chromeBin, UserDataDir: udd, DebugPort: *debugPort, ExtensionDir: ext}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ps, err := c.Profiles()
	if err != nil {
		return err
	}
	logf("profiles: %+v", ps)

	srv := driver.NewServer(fmt.Sprintf("127.0.0.1:%d", *port))
	go func() { _ = srv.ListenAndServe(ctx) }()

	open, err := c.Open(ctx, *profile)
	if err != nil {
		return fmt.Errorf("chrome_open: %w", err)
	}
	logf("chrome_open: %+v", *open)

	setup, err := c.Setup(ctx, *profile, *port, "https://studio.youtube.com")
	if err != nil {
		return fmt.Errorf("extension_setup: %w", err)
	}
	logf("extension_setup: %+v", *setup)

	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	conn, err := srv.WaitFor(wctx, func(driver.Hello) bool { return true })
	if err != nil {
		return fmt.Errorf("extension never connected to port %d: %w", *port, err)
	}
	logf("extension connected on port %d: %+v", *port, conn.Hello())

	st, err := c.Status(ctx, *profile)
	if err != nil {
		return fmt.Errorf("extension_status: %w", err)
	}
	logf("extension_status: %+v", *st)

	var href string
	if err := srv.Browser(conn.Hello().InstanceID).Evaluate(ctx, "location.href", &href); err != nil {
		return fmt.Errorf("browser_evaluate through the extension: %w", err)
	}
	logf("browser side works: tab is at %s", href)
	return nil
}
