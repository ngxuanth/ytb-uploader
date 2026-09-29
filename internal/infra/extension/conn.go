package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/google/uuid"
)

const (
	pingInterval = 20 * time.Second
	readTimeout  = 60 * time.Second
	writeTimeout = 10 * time.Second
)

// ErrClosed is returned for calls that were pending, or issued, after the
// extension's socket went away.
var ErrClosed = errors.New("driver: extension connection closed")

// Hello is what the patched extension sends right after the socket opens.
type Hello struct {
	InstanceID string `json:"instanceId"`
	Email      string `json:"email"`
	Version    string `json:"version"`
}

// message is the extension's wire format, in both directions.
type message struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type responsePayload struct {
	RequestID string          `json:"requestId"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *string         `json:"error,omitempty"`
}

// ExtensionError is an error string reported by the extension itself.
type ExtensionError struct {
	Type    string
	Message string
}

func (e *ExtensionError) Error() string { return fmt.Sprintf("%s: %s", e.Type, e.Message) }

// Conn is one extension instance (one Chrome profile) connected to the driver.
type Conn struct {
	ws    *websocket.Conn
	hello Hello

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan responsePayload

	done      chan struct{}
	closeOnce sync.Once
}

func newConn(ws *websocket.Conn, hello Hello) *Conn {
	return &Conn{
		ws:      ws,
		hello:   hello,
		pending: map[string]chan responsePayload{},
		done:    make(chan struct{}),
	}
}

func (c *Conn) Hello() Hello { return c.hello }

func (c *Conn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Done is closed once the socket is gone.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Call sends one request and waits for its messageResponse. The extension has
// no request timeout of its own, so ctx must carry one.
func (c *Conn) Call(ctx context.Context, typ string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("driver: marshal %s payload: %w", typ, err)
	}
	id := uuid.NewString()
	ch := make(chan responsePayload, 1)

	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(message{ID: id, Type: typ, Payload: raw}); err != nil {
		c.close()
		return fmt.Errorf("driver: send %s: %w", typ, err)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("driver: %s: %w", typ, ctx.Err())
	case <-c.done:
		return ErrClosed
	case resp := <-ch:
		if resp.Error != nil {
			return &ExtensionError{Type: typ, Message: *resp.Error}
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("driver: decode %s result: %w", typ, err)
			}
		}
		return nil
	}
}

func (c *Conn) write(m message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// readLoop dispatches responses until the socket fails.
func (c *Conn) readLoop() {
	defer c.close()
	_ = c.ws.SetReadDeadline(time.Now().Add(readTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(readTimeout))
	})
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(readTimeout))
		var m message
		if json.Unmarshal(data, &m) != nil || m.Type != "messageResponse" {
			continue
		}
		var resp responsePayload
		if json.Unmarshal(m.Payload, &resp) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[resp.RequestID]
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

// pingLoop keeps the socket alive; the browser answers pings on its own.
func (c *Conn) pingLoop() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.writeMu.Lock()
			err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
			c.writeMu.Unlock()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *Conn) close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		close(c.done)
		c.mu.Unlock()
		_ = c.ws.Close()
	})
}
