// Package taskmcp is the MCP surface an LLM session uses to receive its task
// and report back: task_claim, task_report, task_video_created, task_finish.
// Every answer carries a control field so the server can stop the session at
// the next tool call. The same handler is mounted by the server and by the
// launcher's local "try" mode; only the Backend differs.
package taskmcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

// Control tells the LLM whether to keep going.
type Control string

const (
	Continue Control = "continue"
	Stop     Control = "stop"
)

// Session is who is calling, resolved from the bearer token.
type Session struct {
	ID        string
	ProfileID string
}

// Kind of work a session was started for.
const (
	KindUpload      = "upload"
	KindDeleteVideo = "delete_video"
	// KindCheckVideo reads a video's state in Studio (visibility,
	// processing, restrictions) and reports it with task_video_state.
	KindCheckVideo = "check_video"
)

type ClaimOut struct {
	Control Control `json:"control"`
	Message string  `json:"message,omitempty"`
	Kind    string  `json:"kind,omitempty" jsonschema:"upload or delete_video"`
	TaskID  string  `json:"task_id,omitempty"`
	Attempt int     `json:"attempt,omitempty"`
	// FilePath is relative to the browser_upload_file directory.
	FilePath      string `json:"file_path,omitempty" jsonschema:"path to pass to browser_upload_file"`
	ThumbnailPath string `json:"thumbnail_path,omitempty"`
	ChannelID     string `json:"channel_id,omitempty" jsonschema:"YouTube channel id (UC...) to upload to"`
	// ProfileDirectory is the Chrome profile to open (chrome tools).
	ProfileDirectory string         `json:"profile_directory,omitempty" jsonschema:"Chrome profile directory to open with the chrome tools"`
	Metadata         *wire.Metadata `json:"metadata,omitempty"`
	// ExistingVideoID is set when an earlier attempt already created the video.
	ExistingVideoID string `json:"existing_video_id,omitempty" jsonschema:"video already created by an earlier attempt: finish it, do not upload again"`
}

type ReportIn struct {
	TaskID   string `json:"task_id"`
	Step     string `json:"step" jsonschema:"one of PREPARING, ATTACHING, FILLING_METADATA, UPLOADING, PROCESSING, PUBLISHING"`
	Progress int    `json:"progress,omitempty" jsonschema:"upload percent 0-100 while UPLOADING"`
	Message  string `json:"message,omitempty" jsonschema:"one short sentence on what you are doing"`
}

type VideoCreatedIn struct {
	TaskID   string `json:"task_id"`
	VideoID  string `json:"video_id" jsonschema:"the 11-character id from the video link in the upload dialog"`
	VideoURL string `json:"video_url,omitempty"`
}

type FinishIn struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status" jsonschema:"done, failed or needs_attention"`
	VideoID   string `json:"video_id,omitempty"`
	VideoURL  string `json:"video_url,omitempty"`
	ErrorCode string `json:"error_code,omitempty" jsonschema:"for failed/needs_attention: LOGIN_REQUIRED, WRONG_CHANNEL, UPLOAD_LIMIT, CAPTCHA, PLAYLIST_NOT_FOUND, STEP_FAILED"`
	Reason    string `json:"reason,omitempty" jsonschema:"what went wrong, one or two sentences"`
}

// VideoStateIn is what the script read about the task's video in Studio.
type VideoStateIn struct {
	TaskID string `json:"task_id"`
	wire.VideoState
}

type Ack struct {
	Control Control `json:"control"`
	Message string  `json:"message,omitempty"`
}

// Backend is implemented by the server (Postgres) and by the launcher's
// local try mode.
type Backend interface {
	Authenticate(ctx context.Context, token string) (*Session, error)
	Claim(ctx context.Context, s *Session) (*ClaimOut, error)
	Report(ctx context.Context, s *Session, in ReportIn) (*Ack, error)
	VideoCreated(ctx context.Context, s *Session, in VideoCreatedIn) (*Ack, error)
	Finish(ctx context.Context, s *Session, in FinishIn) (*Ack, error)
	VideoState(ctx context.Context, s *Session, in VideoStateIn) (*Ack, error)
}

// ErrUnauthorized is returned by Authenticate for unknown or ended sessions.
var ErrUnauthorized = errors.New("taskmcp: unauthorized")

type ctxKey struct{}

// Tool sets served by one HTTP handler.
type tools uint

const (
	ToolsClaim tools = 1 << iota
	ToolsReport
	ToolsVideo
	ToolsFinish
	ToolsAll = ToolsClaim | ToolsReport | ToolsVideo | ToolsFinish
)

// NewHandler serves every task tool. NewTaskHandler and NewReportHandler
// split them across the two MCP servers the LLM sees.
func NewHandler(b Backend) http.Handler { return newHandler(b, ToolsAll, "uploader") }

// NewTaskHandler serves task_claim.
func NewTaskHandler(b Backend) http.Handler { return newHandler(b, ToolsClaim, "task_mcp") }

// NewReportHandler serves task_report, task_video_created and task_finish.
func NewReportHandler(b Backend) http.Handler {
	return newHandler(b, ToolsReport|ToolsVideo|ToolsFinish, "report_mcp")
}

// newHandler serves MCP over streamable HTTP (stateless). Each request is
// authenticated with "Authorization: Bearer <session token>".
func newHandler(b Backend, set tools, name string) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		s, _ := r.Context().Value(ctxKey{}).(*Session)
		return newServer(b, s, set, name)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || token == r.Header.Get("Authorization") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		s, err := b.Authenticate(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid session token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	})
}

// TokenEqual compares tokens in constant time.
func TokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newServer(b Backend, s *Session, set tools, name string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.0.0"}, nil)

	if set&ToolsClaim != 0 {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "task_claim",
			Description: "Get the task this session must do. Call it first, once. If control is \"stop\" or there is no task, end the session.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *ClaimOut, error) {
			out, err := b.Claim(ctx, s)
			return nil, out, err
		})
	}
	if set&ToolsReport != 0 {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "task_report",
			Description: "Report the step you are on (and upload percent while uploading). Call it whenever the step changes and every ~20s while waiting. If control is \"stop\", stop immediately and end the session.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReportIn) (*mcp.CallToolResult, *Ack, error) {
			out, err := b.Report(ctx, s, in)
			return nil, out, err
		})
	}
	if set&ToolsVideo != 0 {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "task_video_created",
			Description: "Call this as soon as the upload dialog shows the video link, before filling any details.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in VideoCreatedIn) (*mcp.CallToolResult, *Ack, error) {
			out, err := b.VideoCreated(ctx, s, in)
			return nil, out, err
		})
	}
	if set&ToolsVideo != 0 {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "task_video_state",
			Description: "Record what YouTube Studio shows about the task's video: visibility, processing state of each resolution, restrictions, title.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in VideoStateIn) (*mcp.CallToolResult, *Ack, error) {
			out, err := b.VideoState(ctx, s, in)
			return nil, out, err
		})
	}
	if set&ToolsFinish != 0 {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "task_finish",
			Description: "End the task. status=done only after the video was saved/published; otherwise failed or needs_attention with error_code and reason. After this call, end the session.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in FinishIn) (*mcp.CallToolResult, *Ack, error) {
			out, err := b.Finish(ctx, s, in)
			return nil, out, err
		})
	}
	return srv
}
