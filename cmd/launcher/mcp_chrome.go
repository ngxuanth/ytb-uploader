package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/chromectl"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/chromemcp"
)

// Environment of the "chrome" MCP server, set by the launcher per session.
const (
	envChromeBin    = "UPLOADER_CHROME_BIN"
	envUserDataDir  = "UPLOADER_USER_DATA_DIR"
	envDebugPort    = "UPLOADER_DEBUG_PORT"
	envExtensionDir = "UPLOADER_EXTENSION_DIR"
	envProfileDir   = "UPLOADER_PROFILE_DIR"
	envWSPort       = "BMCP_WS_PORT"
)

// mcpChrome serves the chrome tools over stdio; the LLM CLI spawns it.
func mcpChrome() error {
	get := func(k string) (string, error) {
		v := os.Getenv(k)
		if v == "" {
			return "", fmt.Errorf("%s is not set", k)
		}
		return v, nil
	}
	var vals [6]string
	for i, k := range []string{envChromeBin, envUserDataDir, envDebugPort, envExtensionDir, envProfileDir, envWSPort} {
		v, err := get(k)
		if err != nil {
			return err
		}
		vals[i] = v
	}
	debugPort, err := strconv.Atoi(vals[2])
	if err != nil {
		return fmt.Errorf("%s: %w", envDebugPort, err)
	}
	wsPort, err := strconv.Atoi(vals[5])
	if err != nil {
		return fmt.Errorf("%s: %w", envWSPort, err)
	}
	srv := chromemcp.NewServer(chromemcp.Config{
		Controller: &chromectl.Controller{Bin: vals[0], UserDataDir: vals[1], DebugPort: debugPort, ExtensionDir: vals[3]},
		Profile:    vals[4],
		WSPort:     wsPort,
	})
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}
