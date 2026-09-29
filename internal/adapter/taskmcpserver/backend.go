// Package taskmcpserver serves task_mcp / report_mcp from the task service:
// it turns the MCP tool calls into use-case calls and back.
package taskmcpserver

import (
	"context"
	"errors"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/taskservice"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// Backend adapts svc to taskmcp.Backend.
func Backend(svc *taskservice.Service) taskmcp.Backend { return backend{svc} }

type backend struct{ svc *taskservice.Service }

func (b backend) Authenticate(_ context.Context, token string) (*taskmcp.Session, error) {
	id, err := b.svc.Authenticate(token)
	if err != nil {
		return nil, mcpErr(err)
	}
	return &taskmcp.Session{ID: id}, nil
}

func sessionID(s *taskmcp.Session) (string, error) {
	if s == nil {
		return "", errors.New("missing session")
	}
	return s.ID, nil
}

func ack(stop bool) *taskmcp.Ack {
	if stop {
		return &taskmcp.Ack{Control: taskmcp.Stop, Message: "stop"}
	}
	return &taskmcp.Ack{Control: taskmcp.Continue}
}

func mcpErr(err error) error {
	if taskservice.KindOf(err) == taskservice.Unauthorized {
		return taskmcp.ErrUnauthorized
	}
	return err
}

func (b backend) Claim(_ context.Context, s *taskmcp.Session) (*taskmcp.ClaimOut, error) {
	id, err := sessionID(s)
	if err != nil {
		return nil, err
	}
	spec, stop, err := b.svc.Claim(id)
	if err != nil {
		return nil, mcpErr(err)
	}
	if stop {
		return &taskmcp.ClaimOut{Control: taskmcp.Stop, Message: "stopped"}, nil
	}
	return &taskmcp.ClaimOut{
		Control: taskmcp.Continue, Kind: spec.Kind, TaskID: spec.TaskID, Attempt: spec.Attempt,
		FilePath: spec.FilePath, ThumbnailPath: spec.ThumbnailPath, ChannelID: spec.ChannelID,
		ProfileDirectory: spec.ProfileDirectory, Metadata: spec.Metadata, ExistingVideoID: spec.ExistingVideoID,
	}, nil
}

func (b backend) Report(_ context.Context, s *taskmcp.Session, in taskmcp.ReportIn) (*taskmcp.Ack, error) {
	id, err := sessionID(s)
	if err != nil {
		return nil, err
	}
	stop, err := b.svc.Report(id, in.Step, in.Progress, in.Message)
	if err != nil {
		return nil, mcpErr(err)
	}
	return ack(stop), nil
}

func (b backend) VideoCreated(_ context.Context, s *taskmcp.Session, in taskmcp.VideoCreatedIn) (*taskmcp.Ack, error) {
	id, err := sessionID(s)
	if err != nil {
		return nil, err
	}
	stop, err := b.svc.VideoCreated(id, in.VideoID, in.VideoURL)
	if err != nil {
		return nil, mcpErr(err)
	}
	return ack(stop), nil
}

func (b backend) Finish(_ context.Context, s *taskmcp.Session, in taskmcp.FinishIn) (*taskmcp.Ack, error) {
	id, err := sessionID(s)
	if err != nil {
		return nil, err
	}
	if err := b.svc.Finish(id, task.FinishInfo{
		Status: in.Status, ErrorCode: in.ErrorCode, Reason: in.Reason, VideoID: in.VideoID, VideoURL: in.VideoURL,
	}); err != nil {
		return nil, mcpErr(err)
	}
	return &taskmcp.Ack{Control: taskmcp.Stop, Message: "recorded"}, nil
}

func (b backend) VideoState(_ context.Context, s *taskmcp.Session, in taskmcp.VideoStateIn) (*taskmcp.Ack, error) {
	id, err := sessionID(s)
	if err != nil {
		return nil, err
	}
	stop, err := b.svc.VideoState(id, in.VideoState)
	if err != nil {
		return nil, mcpErr(err)
	}
	return ack(stop), nil
}
