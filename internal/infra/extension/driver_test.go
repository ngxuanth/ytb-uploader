package extension

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
)

// fakeExtension behaves like the patched extension: it says hello and
// answers each request with handler's result.
func fakeExtension(t *testing.T, addr, instanceID string, handler func(typ string, payload json.RawMessage) (any, string)) *websocket.Conn {
	t.Helper()
	var ws *websocket.Conn
	var err error
	for i := 0; i < 50; i++ {
		ws, _, err = websocket.DefaultDialer.Dial("ws://"+addr, nil)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hello, _ := json.Marshal(map[string]any{"id": "h", "type": "hello", "payload": Hello{InstanceID: instanceID, Email: instanceID + "@x"}})
	if err := ws.WriteMessage(websocket.TextMessage, hello); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m message
			_ = json.Unmarshal(data, &m)
			if handler == nil {
				continue
			}
			res, errMsg := handler(m.Type, m.Payload)
			p := map[string]any{"requestId": m.ID}
			if errMsg != "" {
				p["error"] = errMsg
			} else if res != nil {
				p["result"] = res
			}
			out, _ := json.Marshal(map[string]any{"id": "r", "type": "messageResponse", "payload": p})
			_ = ws.WriteMessage(websocket.TextMessage, out)
		}
	}()
	return ws
}

func startServer(t *testing.T) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	s := NewServer(addr)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.ListenAndServe(ctx) }()
	return s, addr
}

func waitOnline(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.WaitFor(ctx, func(h Hello) bool { return h.InstanceID == id }); err != nil {
		t.Fatalf("instance %s never came online: %v", id, err)
	}
}

func TestMultipleInstancesAreRoutedByHello(t *testing.T) {
	s, addr := startServer(t)
	fakeExtension(t, addr, "a", func(typ string, _ json.RawMessage) (any, string) { return "url-a", "" })
	fakeExtension(t, addr, "b", func(typ string, _ json.RawMessage) (any, string) { return "url-b", "" })
	waitOnline(t, s, "a")
	waitOnline(t, s, "b")

	for id, want := range map[string]string{"a": "url-a", "b": "url-b"} {
		got, err := s.Browser(id).URL(context.Background())
		if err != nil || got != want {
			t.Fatalf("instance %s: got %q, %v; want %q", id, got, err, want)
		}
	}
	if n := len(s.Online()); n != 2 {
		t.Fatalf("online = %d, want 2", n)
	}
}

func TestEvaluateDecodesResultAndErrors(t *testing.T) {
	s, addr := startServer(t)
	fakeExtension(t, addr, "a", func(typ string, p json.RawMessage) (any, string) {
		var in struct{ Expression string }
		_ = json.Unmarshal(p, &in)
		if in.Expression == "boom" {
			return nil, "Evaluate error: boom"
		}
		return map[string]any{"pct": 42}, ""
	})
	waitOnline(t, s, "a")
	b := s.Browser("a")

	var out struct{ Pct int }
	if err := b.Evaluate(context.Background(), "x", &out); err != nil || out.Pct != 42 {
		t.Fatalf("got %+v, %v", out, err)
	}
	var extErr *ExtensionError
	if err := b.Evaluate(context.Background(), "boom", nil); !errors.As(err, &extErr) {
		t.Fatalf("want ExtensionError, got %v", err)
	}
}

func TestCallTimesOutWhenExtensionNeverAnswers(t *testing.T) {
	s, addr := startServer(t)
	fakeExtension(t, addr, "a", nil)
	waitOnline(t, s, "a")
	err := s.Browser("a").Call(context.Background(), 100*time.Millisecond, "getUrl", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestDisconnectFailsPendingCallAndReconnectIsPickedUp(t *testing.T) {
	s, addr := startServer(t)
	ws := fakeExtension(t, addr, "a", nil)
	waitOnline(t, s, "a")

	errc := make(chan error, 1)
	go func() { errc <- s.Browser("a").Call(context.Background(), 5*time.Second, "getUrl", nil, nil) }()
	time.Sleep(100 * time.Millisecond)
	_ = ws.Close()
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := s.Conn("a"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("instance never went offline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		fakeExtension(t, addr, "a", func(string, json.RawMessage) (any, string) { return "back", "" })
	}()
	got, err := s.Browser("a").URL(context.Background())
	if err != nil || got != "back" {
		t.Fatalf("after reconnect got %q, %v", got, err)
	}
}
