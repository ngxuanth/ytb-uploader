// Package cdp is a minimal Chrome DevTools Protocol client over the
// browser-level WebSocket, with flattened target sessions.
package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
)

type Conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	done    chan struct{}
	err     error
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// BrowserWSURL reads the browser endpoint from /json/version.
func BrowserWSURL(ctx context.Context, port int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WS == "" {
		return "", fmt.Errorf("cdp: no browser websocket on port %d", port)
	}
	return v.WS, nil
}

// Dial connects to the browser target on the debugging port.
func Dial(ctx context.Context, port int) (*Conn, error) {
	u, err := BrowserWSURL(ctx, port)
	if err != nil {
		return nil, err
	}
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := d.DialContext(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(64 << 20)
	c := &Conn{ws: ws, pending: map[int64]chan response{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *Conn) readLoop() {
	defer func() {
		c.mu.Lock()
		close(c.done)
		c.mu.Unlock()
	}()
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.err = err
			return
		}
		var m struct {
			ID int64 `json:"id"`
			response
		}
		if json.Unmarshal(data, &m) != nil || m.ID == 0 {
			continue // events are not used
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.response
		}
	}
}

func (c *Conn) Close() error { return c.ws.Close() }

// Call sends one command, on the browser or on a flattened session.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params any, out any) error {
	if params == nil {
		params = struct{}{}
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return fmt.Errorf("cdp: connection closed: %v", c.err)
	default:
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	c.writeMu.Lock()
	err := c.ws.WriteJSON(msg)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("cdp: connection closed: %v", c.err)
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("cdp %s: %s %s", method, r.Error.Message, r.Error.Data)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

type TargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	BrowserContextID string `json:"browserContextId"`
}

func (c *Conn) Targets(ctx context.Context) ([]TargetInfo, error) {
	var r struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	err := c.Call(ctx, "", "Target.getTargets", nil, &r)
	return r.TargetInfos, err
}

// Attach opens a flattened session on a target.
func (c *Conn) Attach(ctx context.Context, targetID string) (string, error) {
	var r struct {
		SessionID string `json:"sessionId"`
	}
	err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &r)
	return r.SessionID, err
}

func (c *Conn) Detach(ctx context.Context, sessionID string) {
	_ = c.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sessionID}, nil)
}

// Eval runs an expression (promises awaited) and decodes its value.
func (c *Conn) Eval(ctx context.Context, sessionID, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := c.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true,
	}, &r); err != nil {
		return err
	}
	if e := r.ExceptionDetails; e != nil {
		msg := e.Text
		if e.Exception != nil && e.Exception.Description != "" {
			msg = e.Exception.Description
		}
		return fmt.Errorf("cdp eval: %s", msg)
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}
