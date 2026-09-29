package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

func TestUploadAPIAssignsAfterHello(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(video, []byte("video-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(dir, "profile")
	if err := os.MkdirAll(filepath.Join(profiles, "isophtalic"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A real profile has Preferences; Chrome's own folders do not.
	if err := os.WriteFile(filepath.Join(profiles, "isophtalic", "Preferences"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(profiles, "Safe Browsing"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newState("http://"+ln.Addr().String(), profiles, filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = st.app().Listener(ln) }()
	t.Cleanup(func() { ln.Close() })
	time.Sleep(50 * time.Millisecond)

	base := "http://" + ln.Addr().String()
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello, _ := wire.NewEnvelope(wire.MsgHello, wire.Hello{
		AgentID: "t", Version: "test",
		Profiles: []wire.ProfileState{{Directory: "isophtalic", Online: true}},
	})
	if err := conn.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}

	listedBody := getBody(t, base+"/profiles")
	var listed struct {
		Profiles []string `json:"profiles"`
	}
	if err := json.Unmarshal(listedBody, &listed); err != nil || len(listed.Profiles) != 1 || listed.Profiles[0] != "isophtalic" {
		t.Fatalf("profiles: %s", listedBody)
	}

	missing, _ := json.Marshal(uploadIn{Profile: "no-such", Channel: "UCtest", Video: video})
	missResp, err := http.Post(base+"/uploads", "application/json", bytes.NewReader(missing))
	if err != nil {
		t.Fatal(err)
	}
	missBody, _ := io.ReadAll(missResp.Body)
	missResp.Body.Close()
	if missResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(missBody), "unknown profile") {
		t.Fatalf("missing profile: %s %s", missResp.Status, missBody)
	}

	payload, _ := json.Marshal(uploadIn{
		Profile: "isophtalic", Channel: "UCtest", Video: video, Title: "Title", Description: "About",
	})
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for {
		resp, err = http.Post(base+"/uploads", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			break
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("agent did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload status %s", resp.Status)
	}

	var env wire.Envelope
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatal(err)
	}
	if env.Type != wire.MsgAssign {
		t.Fatalf("type %s", env.Type)
	}
	var assign wire.Assign
	if err := json.Unmarshal(env.Data, &assign); err != nil {
		t.Fatal(err)
	}
	if assign.Task.ProfileDirectory != "isophtalic" || assign.Task.FileExt != ".mp4" || assign.TaskMCP.Token == "" {
		t.Fatalf("assign: %+v", assign)
	}
	fileResp, err := http.Get(assign.Task.FileURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(fileResp.Body)
	fileResp.Body.Close()
	if fileResp.StatusCode != 200 || string(body) != "video-bytes" {
		t.Fatalf("file: %s %q", fileResp.Status, body)
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: assign.TaskMCP.URL, HTTPClient: &http.Client{Transport: bearer(assign.TaskMCP.Token)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_claim", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("claim: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var claim taskmcp.ClaimOut
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	if claim.FilePath != "video.mp4" || claim.ChannelID != "UCtest" || claim.Metadata == nil || claim.Metadata.Title != "Title" || claim.ProfileDirectory != "isophtalic" {
		t.Fatalf("claim: %+v", claim)
	}

	rs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: assign.ReportMCP.URL, HTTPClient: &http.Client{Transport: bearer(assign.ReportMCP.Token)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	rep, err := rs.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_report", Arguments: map[string]any{
		"task_id": claim.TaskID, "step": "PREPARING", "message": "starting",
	}})
	if err != nil || rep.IsError {
		t.Fatalf("report: %v %+v", err, rep)
	}
}

func getBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// harness is a running server with a fake agent connected over WebSocket.
type harness struct {
	t     *testing.T
	base  string
	conn  *websocket.Conn
	video string
	data  string
	st    *state
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(video, []byte("video-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(dir, "profile")
	if err := os.MkdirAll(filepath.Join(profiles, "isophtalic"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A real profile has Preferences; Chrome's own folders do not.
	if err := os.WriteFile(filepath.Join(profiles, "isophtalic", "Preferences"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(profiles, "Safe Browsing"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "tasks.json")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newState("http://"+ln.Addr().String(), profiles, data)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = st.app().Listener(ln) }()
	t.Cleanup(func() { ln.Close() })
	base := "http://" + ln.Addr().String()
	var conn *websocket.Conn
	for i := 0; ; i++ {
		conn, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
		if err == nil {
			break
		}
		if i > 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { conn.Close() })
	h := &harness{t: t, base: base, conn: conn, video: video, data: data, st: st}
	h.send(wire.MsgHello, wire.Hello{AgentID: "t", Profiles: []wire.ProfileState{{Directory: "isophtalic", Online: true}}})
	for i := 0; ; i++ {
		var ag struct {
			Connected bool `json:"connected"`
			Agent     *agentInfo
		}
		_ = json.Unmarshal(getBody(t, base+"/agent"), &ag)
		if ag.Connected && ag.Agent != nil {
			break
		}
		if i > 50 {
			t.Fatal("agent not registered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h
}

func (h *harness) send(typ string, data any) {
	h.t.Helper()
	env, _ := wire.NewEnvelope(typ, data)
	if err := h.conn.WriteJSON(env); err != nil {
		h.t.Fatal(err)
	}
}

// expect reads the next message from the server and checks its type.
func (h *harness) expect(typ string, out any) {
	h.t.Helper()
	var env wire.Envelope
	_ = h.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := h.conn.ReadJSON(&env); err != nil {
		h.t.Fatal(err)
	}
	if env.Type != typ {
		h.t.Fatalf("got %s, want %s", env.Type, typ)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) post(path string, body any, wantStatus int) taskView {
	h.t.Helper()
	var r io.Reader = http.NoBody
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	resp, err := http.Post(h.base+path, "application/json", r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		h.t.Fatalf("POST %s: %s %s", path, resp.Status, raw)
	}
	var v taskView
	_ = json.Unmarshal(raw, &v)
	return v
}

func (h *harness) task(id string) taskView {
	h.t.Helper()
	var v taskView
	if err := json.Unmarshal(getBody(h.t, h.base+"/tasks/"+id), &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

// call invokes one task_mcp / report_mcp tool with the assignment's token.
func (h *harness) call(ep wire.MCPEndpoint, tool string, args map[string]any) map[string]any {
	h.t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ep.URL, HTTPClient: &http.Client{Transport: bearer(ep.Token)},
	}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil || res.IsError {
		h.t.Fatalf("%s: %v %+v", tool, err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func events(v taskView) []string {
	var out []string
	for _, e := range v.Events {
		out = append(out, e.Event)
	}
	return out
}

func TestUploadWithoutChannelUsesProfileDefault(t *testing.T) {
	h := newHarness(t)
	created := h.post("/uploads", uploadIn{Profile: "isophtalic", Video: h.video, Title: "Title"}, http.StatusAccepted)
	if created.Status != wire.StatusAssigned || created.Channel != "" {
		t.Fatalf("created: %+v", created)
	}
	var assign wire.Assign
	h.expect(wire.MsgAssign, &assign)
	claim := h.call(assign.TaskMCP, "task_claim", map[string]any{})
	if claim["channel_id"] != nil || claim["profile_directory"] != "isophtalic" {
		t.Fatalf("claim: %v", claim)
	}
}

func TestTaskLifecycleIsTrackedFromReportMCP(t *testing.T) {
	h := newHarness(t)
	created := h.post("/uploads", uploadIn{Profile: "isophtalic", Channel: "UCtest", Video: h.video, Title: "Title"}, http.StatusAccepted)
	if created.Status != wire.StatusAssigned || created.TaskID == "" {
		t.Fatalf("created: %+v", created)
	}
	var assign wire.Assign
	h.expect(wire.MsgAssign, &assign)

	h.call(assign.TaskMCP, "task_claim", map[string]any{})
	h.call(assign.ReportMCP, "task_report", map[string]any{"task_id": created.TaskID, "step": "UPLOADING", "progress": 40, "message": "uploading"})
	if v := h.task(created.TaskID); v.Status != wire.StatusUploading || v.Progress != 40 || v.FinishReported {
		t.Fatalf("while uploading: %+v", v)
	}
	h.call(assign.ReportMCP, "task_video_created", map[string]any{"task_id": created.TaskID, "video_id": "abcdefghijk"})
	ack := h.call(assign.ReportMCP, "task_finish", map[string]any{"task_id": created.TaskID, "status": "done", "video_id": "abcdefghijk"})
	if ack["control"] != "stop" {
		t.Fatalf("finish ack: %v", ack)
	}
	v := h.task(created.TaskID)
	if v.Status != wire.StatusDone || !v.FinishReported || v.Finish.Status != "done" || v.VideoID != "abcdefghijk" || v.VideoURL != "https://youtu.be/abcdefghijk" {
		t.Fatalf("after finish: %+v", v)
	}
	want := "queued assigned task_claim task_report task_video_created task_finish"
	if got := strings.Join(events(v), " "); got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
	// A late report after finish is recorded but gets "stop".
	if ack := h.call(assign.ReportMCP, "task_report", map[string]any{"task_id": created.TaskID, "step": "PUBLISHING"}); ack["control"] != "stop" {
		t.Fatalf("late report: %v", ack)
	}
	h.post("/tasks/"+created.TaskID+"/retry", nil, http.StatusConflict)

	var list struct {
		Tasks []taskView `json:"tasks"`
	}
	_ = json.Unmarshal(getBody(t, h.base+"/tasks?status=done"), &list)
	if len(list.Tasks) != 1 || list.Tasks[0].TaskID != created.TaskID || list.Tasks[0].Events != nil {
		t.Fatalf("list: %+v", list)
	}
}

func TestSessionEndWithoutFinishIsLostAndRetryReusesVideo(t *testing.T) {
	h := newHarness(t)
	created := h.post("/uploads", uploadIn{Profile: "isophtalic", Channel: "UCtest", Video: h.video}, http.StatusAccepted)
	var assign wire.Assign
	h.expect(wire.MsgAssign, &assign)
	h.call(assign.TaskMCP, "task_claim", map[string]any{})
	h.call(assign.ReportMCP, "task_video_created", map[string]any{"task_id": created.TaskID, "video_id": "abcdefghijk"})

	h.send(wire.MsgEvent, wire.Event{
		TaskRef: wire.TaskRef{TaskID: created.TaskID, Attempt: 1}, Type: wire.EventSessionEnded,
		Data: map[string]any{"exit_code": 130, "killed_by": "timeout"}, At: time.Now(),
	})
	var v taskView
	for i := 0; ; i++ {
		if v = h.task(created.TaskID); v.SessionEnded {
			break
		}
		if i > 50 {
			t.Fatal("session_ended not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v.Status != wire.StatusLost || v.FinishReported || v.ErrorCode != wire.ErrAgentLost || v.Session.ExitCode != 130 || v.Session.KilledBy != "timeout" {
		t.Fatalf("lost: %+v", v)
	}

	retried := h.post("/tasks/"+created.TaskID+"/retry", nil, http.StatusAccepted)
	if retried.Attempt != 2 || retried.Status != wire.StatusAssigned || retried.ExistingVideoID != "abcdefghijk" || retried.SessionEnded {
		t.Fatalf("retried: %+v", retried)
	}
	var again wire.Assign
	h.expect(wire.MsgAssign, &again)
	if again.Task.Attempt != 2 || again.TaskMCP.Token == assign.TaskMCP.Token {
		t.Fatalf("second assign: %+v", again)
	}
	claim := h.call(again.TaskMCP, "task_claim", map[string]any{})
	if claim["existing_video_id"] != "abcdefghijk" || claim["attempt"] != float64(2) {
		t.Fatalf("claim: %v", claim)
	}

	cancelled := h.post("/tasks/"+created.TaskID+"/cancel", nil, http.StatusOK)
	if cancelled.Status != wire.StatusCancelled {
		t.Fatalf("cancelled: %+v", cancelled)
	}
	var cancel wire.Cancel
	h.expect(wire.MsgCancel, &cancel)
	if cancel.TaskID != created.TaskID || cancel.Attempt != 2 {
		t.Fatalf("cancel msg: %+v", cancel)
	}
	if ack := h.call(again.ReportMCP, "task_report", map[string]any{"task_id": created.TaskID, "step": "UPLOADING"}); ack["control"] != "stop" {
		t.Fatalf("report after cancel: %v", ack)
	}

	// A restarted server reads the same tasks back.
	st2, err := newState(h.base, h.st.profilesDir, h.data)
	if err != nil {
		t.Fatal(err)
	}
	j := st2.byID[created.TaskID]
	if j == nil || j.Status != wire.StatusCancelled || j.Claim.Attempt != 2 || st2.byToken[again.TaskMCP.Token] != j {
		t.Fatalf("reloaded: %+v", j)
	}
}

func (h *harness) queues() map[string]queueView {
	h.t.Helper()
	var out struct {
		Queues []queueView `json:"queues"`
	}
	if err := json.Unmarshal(getBody(h.t, h.base+"/queues"), &out); err != nil {
		h.t.Fatal(err)
	}
	m := map[string]queueView{}
	for _, q := range out.Queues {
		m[q.Profile] = q
	}
	return m
}

func (h *harness) waitTask(id string, ok func(taskView) bool) taskView {
	h.t.Helper()
	for i := 0; ; i++ {
		v := h.task(id)
		if ok(v) {
			return v
		}
		if i > 100 {
			h.t.Fatalf("task %s: %+v", id, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSameProfileTasksRunOneAfterAnother(t *testing.T) {
	h := newHarness(t)
	in := uploadIn{Profile: "isophtalic", Channel: "UCtest", Video: h.video}
	first := h.post("/uploads", in, http.StatusAccepted)
	second := h.post("/uploads", in, http.StatusAccepted)
	third := h.post("/uploads", in, http.StatusAccepted)
	if first.Status != wire.StatusAssigned || second.Status != wire.StatusQueued || second.QueuePosition != 1 || third.QueuePosition != 2 {
		t.Fatalf("publish: %+v / %+v / %+v", first, second, third)
	}
	var a1 wire.Assign
	h.expect(wire.MsgAssign, &a1)
	if a1.Task.TaskID != first.TaskID {
		t.Fatalf("first assign %s", a1.Task.TaskID)
	}
	q := h.queues()["isophtalic"]
	if q.Running != first.TaskID || strings.Join(q.Queued, ",") != second.TaskID+","+third.TaskID || !q.Subscribed {
		t.Fatalf("queues: %+v", q)
	}

	// Cancelling a queued task only takes it out of line.
	if v := h.post("/tasks/"+third.TaskID+"/cancel", nil, http.StatusOK); v.Status != wire.StatusCancelled || v.QueuePosition != 0 {
		t.Fatalf("cancel queued: %+v", v)
	}

	// task_finish alone does not free the profile: the harness may still be
	// using Chrome until it exits.
	h.call(a1.TaskMCP, "task_claim", map[string]any{})
	h.call(a1.ReportMCP, "task_finish", map[string]any{"task_id": first.TaskID, "status": "done"})
	if v := h.task(second.TaskID); v.Status != wire.StatusQueued || v.QueuePosition != 1 {
		t.Fatalf("second before session_ended: %+v", v)
	}
	h.send(wire.MsgEvent, wire.Event{TaskRef: wire.TaskRef{TaskID: first.TaskID, Attempt: 1}, Type: wire.EventSessionEnded, At: time.Now()})
	var a2 wire.Assign
	h.expect(wire.MsgAssign, &a2)
	if a2.Task.TaskID != second.TaskID {
		t.Fatalf("second assign %s", a2.Task.TaskID)
	}
	if v := h.task(second.TaskID); v.Status != wire.StatusAssigned || v.AgentID != "t" {
		t.Fatalf("second: %+v", v)
	}
	if v := h.task(first.TaskID); v.Status != wire.StatusDone || !v.SessionEnded {
		t.Fatalf("first: %+v", v)
	}
	if q := h.queues()["isophtalic"]; q.Running != second.TaskID || len(q.Queued) != 0 {
		t.Fatalf("queues after: %+v", q)
	}
}

func TestQueuedUntilALauncherServesTheProfile(t *testing.T) {
	h := newHarness(t)
	// A second launcher that says hello without the profile makes no
	// difference; a launcher restart (hello without the running task) frees it.
	queued := h.post("/uploads", uploadIn{Profile: "isophtalic", Channel: "UCtest", Video: h.video}, http.StatusAccepted)
	var a wire.Assign
	h.expect(wire.MsgAssign, &a)
	next := h.post("/uploads", uploadIn{Profile: "isophtalic", Channel: "UCtest", Video: h.video}, http.StatusAccepted)
	if next.QueuePosition != 1 {
		t.Fatalf("next: %+v", next)
	}

	// The launcher reconnects without the running session: that task is
	// lost and the next one is assigned.
	h.send(wire.MsgHello, wire.Hello{AgentID: "t", Profiles: []wire.ProfileState{{Directory: "isophtalic", Online: true}}})
	var a2 wire.Assign
	h.expect(wire.MsgAssign, &a2)
	if a2.Task.TaskID != next.TaskID {
		t.Fatalf("assigned %s, want %s", a2.Task.TaskID, next.TaskID)
	}
	lost := h.task(queued.TaskID)
	if lost.Status != wire.StatusLost || lost.ErrorCode != wire.ErrAgentLost {
		t.Fatalf("lost: %+v", lost)
	}
	// A hello that still reports the running task keeps it.
	h.send(wire.MsgHello, wire.Hello{AgentID: "t", Profiles: []wire.ProfileState{{Directory: "isophtalic", Online: true, RunningTaskID: next.TaskID}}})
	h.waitTask(next.TaskID, func(v taskView) bool { return v.Status == wire.StatusAssigned })

	// Retry to the front of the line while the profile is busy: it waits.
	r := h.post("/tasks/"+queued.TaskID+"/retry?front=true", nil, http.StatusAccepted)
	if r.Status != wire.StatusQueued || r.QueuePosition != 1 || r.Attempt != 2 {
		t.Fatalf("retry: %+v", r)
	}
	if q := h.queues()["isophtalic"]; q.Running != next.TaskID || len(q.Queued) != 1 {
		t.Fatalf("queues: %+v", q)
	}
}

func TestUploadWithoutLauncherWaitsInQueue(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	_ = os.WriteFile(video, []byte("v"), 0o600)
	profiles := filepath.Join(dir, "profile")
	_ = os.MkdirAll(filepath.Join(profiles, "p1"), 0o700)
	_ = os.WriteFile(filepath.Join(profiles, "p1", "Preferences"), []byte("{}"), 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newState("http://"+ln.Addr().String(), profiles, filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = st.app().Listener(ln) }()
	t.Cleanup(func() { ln.Close() })
	h := &harness{t: t, base: "http://" + ln.Addr().String()}
	var v taskView
	for i := 0; ; i++ {
		raw, _ := json.Marshal(uploadIn{Profile: "p1", Channel: "UC", Video: video})
		resp, err := http.Post(h.base+"/uploads", "application/json", bytes.NewReader(raw))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			_ = json.Unmarshal(body, &v)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("upload: %s %s", resp.Status, body)
			}
			break
		}
		if i > 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v.Status != wire.StatusQueued || v.QueuePosition != 1 {
		t.Fatalf("queued: %+v", v)
	}
	if q := h.queues()["p1"]; q.Subscribed || q.Running != "" || len(q.Queued) != 1 {
		t.Fatalf("queues: %+v", q)
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	h.conn = conn
	h.send(wire.MsgHello, wire.Hello{AgentID: "late", Profiles: []wire.ProfileState{{Directory: "p1", Online: true}}})
	var a wire.Assign
	h.expect(wire.MsgAssign, &a)
	if a.Task.TaskID != v.TaskID {
		t.Fatalf("assign: %+v", a)
	}
}
