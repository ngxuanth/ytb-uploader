package taskmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

type fakeBackend struct {
	stop    bool
	reports []ReportIn
}

func (f *fakeBackend) Authenticate(_ context.Context, token string) (*Session, error) {
	if token != "good" {
		return nil, ErrUnauthorized
	}
	return &Session{ID: "s1", ProfileID: "p1"}, nil
}

func (f *fakeBackend) control() Control {
	if f.stop {
		return Stop
	}
	return Continue
}

func (f *fakeBackend) Claim(context.Context, *Session) (*ClaimOut, error) {
	return &ClaimOut{Control: f.control(), Kind: KindUpload, TaskID: "t1", FilePath: "v.mp4", ChannelID: "UC1",
		Metadata: &contract.Metadata{Title: "hi", Visibility: contract.VisibilityPrivate}}, nil
}

func (f *fakeBackend) Report(_ context.Context, _ *Session, in ReportIn) (*Ack, error) {
	f.reports = append(f.reports, in)
	return &Ack{Control: f.control()}, nil
}

func (f *fakeBackend) VideoCreated(context.Context, *Session, VideoCreatedIn) (*Ack, error) {
	return &Ack{Control: f.control()}, nil
}

func (f *fakeBackend) Finish(context.Context, *Session, FinishIn) (*Ack, error) {
	return &Ack{Control: Stop}, nil
}

func (f *fakeBackend) VideoState(context.Context, *Session, VideoStateIn) (*Ack, error) {
	return &Ack{Control: f.control()}, nil
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}},
	}, nil)
}

func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args any) T {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned tool error: %+v", name, res.Content)
	}
	var out T
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, b, err)
	}
	return out
}

func TestToolsRoundTripAndStopControl(t *testing.T) {
	be := &fakeBackend{}
	srv := httptest.NewServer(NewHandler(be))
	defer srv.Close()

	cs, err := connect(t, srv.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	claim := call[ClaimOut](t, cs, "task_claim", map[string]any{})
	if claim.TaskID != "t1" || claim.Metadata == nil || claim.Metadata.Title != "hi" || claim.Control != Continue {
		t.Fatalf("claim: %+v", claim)
	}
	ack := call[Ack](t, cs, "task_report", map[string]any{"task_id": "t1", "step": "UPLOADING", "progress": 40})
	if ack.Control != Continue || len(be.reports) != 1 || be.reports[0].Progress != 40 {
		t.Fatalf("report: %+v %+v", ack, be.reports)
	}
	be.stop = true
	if ack := call[Ack](t, cs, "task_report", map[string]any{"task_id": "t1", "step": "UPLOADING"}); ack.Control != Stop {
		t.Fatalf("expected stop after cancel, got %+v", ack)
	}
}

func TestSplitHandlersExposeOneSide(t *testing.T) {
	srv := httptest.NewServer(NewTaskHandler(&fakeBackend{}))
	defer srv.Close()
	cs, err := connect(t, srv.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "task_claim" {
		t.Fatalf("task tools: %v %v", tools, err)
	}

	rep := httptest.NewServer(NewReportHandler(&fakeBackend{}))
	defer rep.Close()
	rs, err := connect(t, rep.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	got, err := rs.ListTools(context.Background(), nil)
	if err != nil || len(got.Tools) != 4 {
		t.Fatalf("report tools: %v %v", got, err)
	}
}

func TestBadTokenIsRejected(t *testing.T) {
	srv := httptest.NewServer(NewHandler(&fakeBackend{}))
	defer srv.Close()
	if cs, err := connect(t, srv.URL, "bad"); err == nil {
		cs.Close()
		t.Fatal("connect with a bad token should fail")
	}
}
