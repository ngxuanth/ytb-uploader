// Package extension replaces the @browsermcp/mcp server: it accepts the Browser
// MCP extension's WebSocket connections (one per Chrome profile) and drives
// them with typed calls.
package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
)

// logger is plain slog: the driver runs inside the launcher CLI, which does
// not boot fountain (whose logger needs its config and log directory).
var logger = slog.Default()

const helloTimeout = 10 * time.Second

// Server listens for extension connections. The extension is a WebSocket
// client, so this is the only listening side.
type Server struct {
	addr string

	mu    sync.RWMutex
	conns map[string]*Conn // by instance id
	subs  map[chan struct{}]struct{}

	// OnChange is called (from a driver goroutine) whenever an instance
	// connects or disconnects.
	OnChange func(h Hello, online bool)
}

func NewServer(addr string) *Server {
	return &Server{
		addr:  addr,
		conns: map[string]*Conn{},
		subs:  map[chan struct{}]struct{}{},
	}
}

var upgrader = websocket.Upgrader{
	// Only extensions on this machine can reach a 127.0.0.1 listener, and
	// their origin is chrome-extension://<id>.
	CheckOrigin: func(*http.Request) bool { return true },
}

// ListenAndServe blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: http.HandlerFunc(s.handle)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			c.close()
		}
		s.mu.Unlock()
	}()
	logger.Info(fmt.Sprintf("driver: listening for extensions on %s", s.addr))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn(fmt.Sprintf("driver: upgrade failed: %v", err))
		return
	}
	_ = ws.SetReadDeadline(time.Now().Add(helloTimeout))
	_, data, err := ws.ReadMessage()
	if err != nil {
		_ = ws.Close()
		return
	}
	var m message
	var h Hello
	if json.Unmarshal(data, &m) != nil || m.Type != "hello" || json.Unmarshal(m.Payload, &h) != nil || h.InstanceID == "" {
		// An unpatched extension never says hello; it cannot be addressed.
		logger.Warn(fmt.Sprintf("driver: connection without hello from %s, closing", r.RemoteAddr))
		_ = ws.Close()
		return
	}

	c := newConn(ws, h)
	s.mu.Lock()
	if old := s.conns[h.InstanceID]; old != nil {
		old.close()
	}
	s.conns[h.InstanceID] = c
	s.notifyLocked()
	s.mu.Unlock()
	logger.Info(fmt.Sprintf("driver: extension connected instance=%s email=%s version=%s", h.InstanceID, h.Email, h.Version))
	if s.OnChange != nil {
		s.OnChange(h, true)
	}

	go c.pingLoop()
	c.readLoop()

	s.mu.Lock()
	current := s.conns[h.InstanceID] == c
	if current {
		delete(s.conns, h.InstanceID)
		s.notifyLocked()
	}
	s.mu.Unlock()
	if current {
		logger.Info(fmt.Sprintf("driver: extension disconnected instance=%s", h.InstanceID))
		if s.OnChange != nil {
			s.OnChange(h, false)
		}
	}
}

func (s *Server) notifyLocked() {
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Conn returns the current connection of an instance.
func (s *Server) Conn(instanceID string) (*Conn, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.conns[instanceID]
	if !ok || !c.alive() {
		return nil, false
	}
	return c, true
}

// Online lists the hello of every connected instance.
func (s *Server) Online() []Hello {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Hello, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c.hello)
	}
	return out
}

// WaitFor blocks until an instance matching fn is connected.
func (s *Server) WaitFor(ctx context.Context, fn func(Hello) bool) (*Conn, error) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	for {
		s.mu.RLock()
		for _, c := range s.conns {
			if c.alive() && fn(c.hello) {
				s.mu.RUnlock()
				return c, nil
			}
		}
		s.mu.RUnlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// Browser addresses one instance by id and survives reconnects: every call
// uses whichever socket that instance currently has.
func (s *Server) Browser(instanceID string) *Browser {
	return &Browser{s: s, id: instanceID}
}
