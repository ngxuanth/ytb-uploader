package main

import (
	"testing"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

func TestReserveRejectsBusyAndDrain(t *testing.T) {
	a := &agent{running: map[string]*runningTask{}}
	msg := contract.Assign{Task: contract.TaskSpec{TaskID: "t1", ProfileDirectory: "Default"}, TaskMCP: contract.MCPEndpoint{URL: "http://task"}, ReportMCP: contract.MCPEndpoint{URL: "http://report"}}
	a.drain = true
	if reason := a.reserve(t.Context(), msg); reason != "agent is draining" {
		t.Fatalf("drain: %q", reason)
	}
	a.drain = false
	a.running["Default"] = &runningTask{taskID: "other"}
	if reason := a.reserve(t.Context(), msg); reason != "profile is busy" {
		t.Fatalf("busy: %q", reason)
	}
	if reason := a.reserve(t.Context(), contract.Assign{}); reason == "" {
		t.Fatal("empty assign should be rejected")
	}
}
