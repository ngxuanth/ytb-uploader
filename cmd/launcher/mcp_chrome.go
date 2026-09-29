package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/chromemcp"
)

// mcpChrome serves the chrome tools over stdio; the LLM CLI spawns it.
func mcpChrome() error {
	cfg, err := chromemcp.ConfigFromEnv()
	if err != nil {
		return err
	}
	return chromemcp.NewServer(cfg).Run(context.Background(), &mcp.StdioTransport{})
}
