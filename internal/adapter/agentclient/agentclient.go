// Package agentclient is the launcher's end of the server WebSocket: it
// stays connected (reconnecting with backoff), says hello, sends heartbeats
// and hands every message to the agent.
package agentclient

import (
	"context"
	"sync"
	"time"

	"github.com/fasthttp/websocket"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/agent"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Run serves a until ctx ends; then every running task is cancelled.
func Run(ctx context.Context, url string, a *agent.Agent, logf func(string, ...any)) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			a.StopAll()
			return nil
		}
		err := serve(ctx, url, a, logf)
		if ctx.Err() != nil {
			a.StopAll()
			return nil
		}
		logf("socket closed: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			a.StopAll()
			return nil
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// conn is an agent.Sender; writes are serialized.
type conn struct {
	mu sync.Mutex
	ws *websocket.Conn
}

func (c *conn) Send(typ string, data any) error {
	env, err := contract.NewEnvelope(typ, data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteJSON(env)
}

func serve(ctx context.Context, url string, a *agent.Agent, logf func(string, ...any)) error {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	c := &conn{ws: ws}
	a.Connected(c)
	defer a.Disconnected(c)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// ReadJSON below does not watch ctx; closing the socket on shutdown is
	// what unblocks it, so Ctrl+C really stops the launcher.
	go func() {
		<-connCtx.Done()
		ws.Close()
	}()
	go heartbeat(connCtx, c, a)
	if err := c.Send(contract.MsgHello, a.Hello()); err != nil {
		return err
	}
	logf("connected to %s", url)
	for {
		var env contract.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return err
		}
		a.Handle(ctx, env)
	}
}

func heartbeat(ctx context.Context, c *conn, a *agent.Agent) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Send(contract.MsgHeartbeat, a.Heartbeat()); err != nil {
				return
			}
		}
	}
}
