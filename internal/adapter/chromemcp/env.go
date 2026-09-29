package chromemcp

import (
	"fmt"
	"os"
	"strconv"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/chrome"
)

// Environment of the "chrome" MCP server, set by the launcher per session.
const (
	EnvChromeBin    = "UPLOADER_CHROME_BIN"
	EnvUserDataDir  = "UPLOADER_USER_DATA_DIR"
	EnvDebugPort    = "UPLOADER_DEBUG_PORT"
	EnvExtensionDir = "UPLOADER_EXTENSION_DIR"
	EnvProfileDir   = "UPLOADER_PROFILE_DIR"
	EnvWSPort       = "BMCP_WS_PORT"
)

// Env is the environment that starts the server for profile on ctl, with
// the extension on wsPort.
func Env(ctl *chrome.Controller, profile string, wsPort int) map[string]string {
	return map[string]string{
		EnvChromeBin: ctl.Bin, EnvUserDataDir: ctl.UserDataDir,
		EnvDebugPort: strconv.Itoa(ctl.DebugPort), EnvExtensionDir: ctl.ExtensionDir,
		EnvProfileDir: profile, EnvWSPort: strconv.Itoa(wsPort),
	}
}

// ConfigFromEnv reads what Env set.
func ConfigFromEnv() (Config, error) {
	var vals [6]string
	for i, k := range []string{EnvChromeBin, EnvUserDataDir, EnvDebugPort, EnvExtensionDir, EnvProfileDir, EnvWSPort} {
		if vals[i] = os.Getenv(k); vals[i] == "" {
			return Config{}, fmt.Errorf("%s is not set", k)
		}
	}
	debugPort, err := strconv.Atoi(vals[2])
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvDebugPort, err)
	}
	wsPort, err := strconv.Atoi(vals[5])
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvWSPort, err)
	}
	return Config{
		Controller: &chrome.Controller{Bin: vals[0], UserDataDir: vals[1], DebugPort: debugPort, ExtensionDir: vals[3]},
		Profile:    vals[4],
		WSPort:     wsPort,
	}, nil
}
