package wire

import (
	"encoding/json"
	"time"
)

// Envelope frames every message on the server <-> agent socket.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

func NewEnvelope(typ string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	return Envelope{Type: typ, Data: raw}, err
}

// Agent -> server message types.
const (
	MsgHello         = "hello"
	MsgProfileUpdate = "profile_update"
	MsgHeartbeat     = "heartbeat"
	MsgAccept        = "accept"
	MsgReject        = "reject"
	MsgEvent         = "event"
	MsgResult        = "result"
)

// Server -> agent message types.
const (
	MsgAssign        = "assign"
	MsgCancel        = "cancel"
	MsgReopenProfile = "reopen_profile"
	MsgDrain         = "drain"
)

// ProfileState is an agent's view of one configured Chrome profile.
type ProfileState struct {
	Directory     string `json:"directory"`
	Email         string `json:"email,omitempty"`
	InstanceID    string `json:"instance_id,omitempty"`
	Online        bool   `json:"online"`
	RunningTaskID string `json:"running_task_id,omitempty"`
}

type Hello struct {
	AgentID  string         `json:"agent_id"`
	Version  string         `json:"version"`
	Profiles []ProfileState `json:"profiles"`
}

type RunningTask struct {
	TaskID  string `json:"task_id"`
	Attempt int    `json:"attempt"`
	Status  Status `json:"status"`
}

type Heartbeat struct {
	Running []RunningTask `json:"running"`
}

// TaskSpec is everything the agent needs to run one attempt.
type TaskSpec struct {
	TaskID           string `json:"task_id"`
	Attempt          int    `json:"attempt"`
	ProfileDirectory string `json:"profile_directory"`
	ChannelID        string `json:"channel_id"` // YouTube UC… id
	FileURL          string `json:"file_url"`
	FileSize         int64  `json:"file_size"`
	SHA256           string `json:"sha256"`
	FileExt          string `json:"file_ext"`
	// Kind is taskmcp's kind (upload, check_video); an empty kind is upload.
	// A check_video task has no file to download.
	Kind         string   `json:"kind,omitempty"`
	ThumbnailURL string   `json:"thumbnail_url,omitempty"`
	Metadata     Metadata `json:"metadata"`
	// ExistingVideoID is set when an earlier attempt already created a video;
	// the agent reconciles it instead of uploading blindly again.
	ExistingVideoID string    `json:"existing_video_id,omitempty"`
	Deadline        time.Time `json:"deadline"`
}

// MCPEndpoint is one HTTP MCP server the harness should call.
type MCPEndpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// Assign tells the agent to prepare one profile and start a harness.
// Task details the LLM needs (title, visibility, channel) stay on task_mcp.
type Assign struct {
	Task      TaskSpec    `json:"task"`
	TaskMCP   MCPEndpoint `json:"task_mcp"`
	ReportMCP MCPEndpoint `json:"report_mcp"`
}

type TaskRef struct {
	TaskID  string `json:"task_id"`
	Attempt int    `json:"attempt"`
}

type Reject struct {
	TaskRef
	Reason string `json:"reason"`
}

type Cancel struct {
	TaskRef
	Cleanup bool `json:"cleanup"`
}

type ReopenProfile struct {
	Directory string `json:"directory"`
}

type Drain struct {
	Enabled bool `json:"enabled"`
}

// Event types inside MsgEvent.
const (
	EventStatus       = "status"
	EventProgress     = "progress"
	EventVideoCreated = "video_created"
	EventLog          = "log"
	EventCleanup      = "cleanup"
	// EventSessionEnded is sent when the harness process of an attempt
	// exits, whether or not it called task_finish. Data holds exit_code,
	// killed_by, duration_ms and error.
	EventSessionEnded = "session_ended"
)

type Event struct {
	TaskRef
	Seq          int64          `json:"seq"`
	Type         string         `json:"type"`
	Status       Status         `json:"status,omitempty"`
	Progress     int            `json:"progress,omitempty"`
	Message      string         `json:"message,omitempty"`
	VideoID      string         `json:"video_id,omitempty"`
	VideoURL     string         `json:"video_url,omitempty"`
	ScreenshotID string         `json:"screenshot_id,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
	At           time.Time      `json:"at"`
}

// Result ends an attempt. Status is one of DONE, FAILED, NEEDS_ATTENTION,
// CANCELLED.
type Result struct {
	TaskRef
	Status       Status    `json:"status"`
	ErrorCode    ErrorCode `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	FailedStep   string    `json:"failed_step,omitempty"`
	VideoID      string    `json:"video_id,omitempty"`
	VideoURL     string    `json:"video_url,omitempty"`
	ScreenshotID string    `json:"screenshot_id,omitempty"`
	At           time.Time `json:"at"`
}
