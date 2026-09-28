// Package chromemcp exposes chromectl to the LLM as the "chrome" MCP
// server: list profiles, open Chrome on the session's profile, set up the
// Browser MCP extension, check it is connected. Tools only act on the one
// profile the session was started for.
package chromemcp

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/chromectl"
)

type Config struct {
	Controller *chromectl.Controller
	Profile    string // the only profile directory this session may use
	WSPort     int    // bmcp port of this session
}

type ProfilesOut struct {
	SessionProfile string              `json:"session_profile" jsonschema:"the profile directory this session must use"`
	Profiles       []chromectl.Profile `json:"profiles"`
}

type ProfileIn struct {
	ProfileDirectory string `json:"profile_directory" jsonschema:"profile directory, e.g. Default or Profile 1"`
}

type SetupIn struct {
	ProfileDirectory string `json:"profile_directory"`
	URL              string `json:"url,omitempty" jsonschema:"page to open in the automation tab (default https://studio.youtube.com)"`
}

func NewServer(cfg Config) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "chrome", Version: "1.0.0"}, nil)
	check := func(dir string) error {
		if dir != cfg.Profile {
			return fmt.Errorf("this session may only use profile %q, not %q", cfg.Profile, dir)
		}
		return nil
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "chrome_profiles",
		Description: "List the Chrome profiles (directory, name, signed-in email) and the one this session must use.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *ProfilesOut, error) {
		ps, err := cfg.Controller.Profiles()
		if err != nil {
			return nil, nil, err
		}
		return nil, &ProfilesOut{SessionProfile: cfg.Profile, Profiles: ps}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "chrome_open",
		Description: "Open Chrome on the profile with remote debugging (restarts the upload Chrome if it runs without it). Safe to call when Chrome is already open.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProfileIn) (*mcp.CallToolResult, *chromectl.OpenResult, error) {
		if err := check(in.ProfileDirectory); err != nil {
			return nil, nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		out, err := cfg.Controller.Open(ctx, in.ProfileDirectory)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "extension_setup",
		Description: "Install the Browser MCP extension in the profile if missing, point it at this session's browser server and open the automation tab. Call after chrome_open.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SetupIn) (*mcp.CallToolResult, *chromectl.SetupResult, error) {
		if err := check(in.ProfileDirectory); err != nil {
			return nil, nil, err
		}
		if in.URL == "" {
			in.URL = "https://studio.youtube.com"
		}
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		out, err := cfg.Controller.Setup(ctx, in.ProfileDirectory, cfg.WSPort, in.URL)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "extension_status",
		Description: "Check whether the profile's extension is connected to this session's browser server, and which tab it drives.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProfileIn) (*mcp.CallToolResult, *chromectl.Status, error) {
		if err := check(in.ProfileDirectory); err != nil {
			return nil, nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		st, err := cfg.Controller.Status(ctx, in.ProfileDirectory)
		if err == nil && st.Installed && st.WSPort != cfg.WSPort {
			st.Message += fmt.Sprintf("; extension points at port %d but this session uses %d (call extension_setup)", st.WSPort, cfg.WSPort)
		}
		return nil, st, err
	})
	return s
}
