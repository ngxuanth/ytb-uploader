// Package session starts one LLM upload and waits until it ends.
// task_mcp and report_mcp are remote (or local stand-ins). chrome_mcp and
// bmcp run on this machine.
package session

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/llm/harness"
)

// Endpoint is one HTTP MCP server the LLM calls.
type Endpoint struct {
	URL   string
	Token string
}

// Config is one harness run. Dir is the session directory; the video is
// already in UploadDir.
type Config struct {
	ID        string
	Dir       string
	UploadDir string
	Prompt    string
	Spec      harness.Spec
	BMCP      string
	Port      int
	Launcher  string
	ChromeEnv map[string]string

	Task   Endpoint
	Report Endpoint

	Timeout time.Duration
	Idle    time.Duration
	IdleFor func() time.Duration

	// Cancel asks the session to stop. OnCancel runs first (so a local
	// backend can start answering control=stop); the process is killed
	// after Grace.
	Cancel   <-chan struct{}
	OnCancel func()
	Grace    time.Duration
	OnLine   harness.LineFunc
	Logf     func(string, ...any)
}

// Outcome is the finished harness process.
type Outcome struct {
	harness.Result
	KilledBy string
}

// Run builds the four MCP servers, starts the CLI and waits.
func Run(ctx context.Context, cfg Config) (Outcome, error) {
	chromeEnv := desktopEnv(cfg.ChromeEnv)
	sessEnv := map[string]string{
		"UPLOADER_TASK_MCP_URL": cfg.Task.URL, "UPLOADER_TASK_MCP_TOKEN": cfg.Task.Token,
		"UPLOADER_REPORT_MCP_URL": cfg.Report.URL, "UPLOADER_REPORT_MCP_TOKEN": cfg.Report.Token,
		"BMCP_WS_PORT": strconv.Itoa(cfg.Port), "BMCP_UPLOAD_DIR": cfg.UploadDir,
		"UPLOADER_BMCP": cfg.BMCP, "UPLOADER_LAUNCHER": cfg.Launcher,
	}
	for k, v := range chromeEnv {
		sessEnv[k] = v
	}
	cmd, err := cfg.Spec.Build(harness.Session{
		Dir:    cfg.Dir,
		Prompt: cfg.Prompt,
		Servers: []harness.MCPServer{
			{Name: "bmcp", Command: "node", Args: []string{cfg.BMCP}, Env: map[string]string{
				"BMCP_WS_PORT": strconv.Itoa(cfg.Port), "BMCP_NO_KILL": "1", "BMCP_UPLOAD_DIR": cfg.UploadDir,
			}},
			{Name: "task_mcp", URL: cfg.Task.URL, Headers: map[string]string{"Authorization": "Bearer " + cfg.Task.Token}},
			{Name: "report_mcp", URL: cfg.Report.URL, Headers: map[string]string{"Authorization": "Bearer " + cfg.Report.Token}},
			{Name: "chrome_mcp", Command: cfg.Launcher, Args: []string{"mcp-chrome"}, Env: chromeEnv},
		},
		Env: sessEnv,
	})
	if err != nil {
		return Outcome{}, err
	}
	transcript := cfg.Dir + "/transcript.jsonl"
	cfg.logf("session %s: %s (transcript %s)", cfg.ID, cfg.Spec.Bin, transcript)
	p, err := harness.Start(cmd, transcript, cfg.OnLine)
	if err != nil {
		return Outcome{}, err
	}
	return wait(ctx, p, cfg), nil
}

func wait(ctx context.Context, p *harness.Process, cfg Config) Outcome {
	var timeout <-chan time.Time
	if cfg.Timeout > 0 {
		timeout = time.After(cfg.Timeout)
	}
	grace := cfg.Grace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var cancelOnce bool

loop:
	for {
		select {
		case <-p.Done():
			break loop
		case <-ctx.Done():
			cfg.logf("interrupted: killing session")
			p.Kill("interrupted", 5*time.Second)
		case <-timeout:
			cfg.logf("timeout: killing session")
			p.Kill("timeout", 5*time.Second)
		case <-cfg.Cancel:
			if cancelOnce {
				continue
			}
			cancelOnce = true
			cfg.logf("cancel requested: kill in %s", grace)
			if cfg.OnCancel != nil {
				cfg.OnCancel()
			}
			go func() {
				select {
				case <-p.Done():
				case <-time.After(grace):
					p.Kill("cancelled", 5*time.Second)
				}
			}()
		case <-tick.C:
			if cfg.Idle > 0 && cfg.IdleFor != nil && cfg.IdleFor() > cfg.Idle {
				cfg.logf("no uploader call for %s: killing session", cfg.Idle)
				p.Kill("stalled", 5*time.Second)
			}
		}
	}
	res, _ := p.Wait(context.Background())
	return Outcome{Result: res, KilledBy: p.KilledBy()}
}

func (cfg Config) logf(format string, a ...any) {
	if cfg.Logf != nil {
		cfg.Logf(format, a...)
	}
}

// desktopEnv copies chrome's env and adds the desktop session variables
// Hermes does not forward on its own.
func desktopEnv(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
	// Chrome runs under X11 (XWayland), which needs XAUTHORITY to connect to
	// the display; without it Chrome exits before opening the debug port.
	for _, k := range []string{"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "XDG_SESSION_TYPE", "DBUS_SESSION_BUS_ADDRESS"} {
		if out[k] != "" {
			continue
		}
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// FreePort returns a TCP port that was free at the moment of the call.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
