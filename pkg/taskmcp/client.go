package taskmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client calls task_mcp and report_mcp the way an LLM session does, so the
// launcher's scripted runner reports through the same tools and the server
// sees no difference.
type Client struct {
	taskURL, taskToken, reportURL, reportToken string
}

func NewClient(taskURL, taskToken, reportURL, reportToken string) *Client {
	return &Client{taskURL: taskURL, taskToken: taskToken, reportURL: reportURL, reportToken: reportToken}
}

func (c *Client) Claim(ctx context.Context) (*ClaimOut, error) {
	var out ClaimOut
	return &out, c.call(ctx, c.taskURL, c.taskToken, "task_claim", map[string]any{}, &out)
}

func (c *Client) Report(ctx context.Context, in ReportIn) (*Ack, error) {
	var out Ack
	return &out, c.call(ctx, c.reportURL, c.reportToken, "task_report", in, &out)
}

func (c *Client) VideoCreated(ctx context.Context, in VideoCreatedIn) (*Ack, error) {
	var out Ack
	return &out, c.call(ctx, c.reportURL, c.reportToken, "task_video_created", in, &out)
}

func (c *Client) Finish(ctx context.Context, in FinishIn) (*Ack, error) {
	var out Ack
	return &out, c.call(ctx, c.reportURL, c.reportToken, "task_finish", in, &out)
}

// call opens a session per call: the servers are stateless and a task makes
// only a few dozen calls.
func (c *Client) call(ctx context.Context, url, token, tool string, args, out any) error {
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "uploader-playbook", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearerAuth{token}},
	}, nil)
	if err != nil {
		return fmt.Errorf("%s: connect: %w", tool, err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	if res.IsError {
		msg := ""
		for _, ct := range res.Content {
			if t, ok := ct.(*mcp.TextContent); ok {
				msg += t.Text
			}
		}
		return fmt.Errorf("%s: %s", tool, msg)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

type bearerAuth struct{ token string }

func (b bearerAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
