package chrome

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const browserMCPName = "Browser MCP Local"

const selectTabJS = `(async () => {
  let [tab] = await chrome.tabs.query({active: true, currentWindow: true});
  if (!tab || tab.id == null) {
    tab = await chrome.tabs.create({url: "https://www.youtube.com/", active: true});
  } else if (!tab.url || !tab.url.startsWith("http")) {
    await chrome.tabs.update(tab.id, {url: "https://www.youtube.com/", active: true});
  }
  // WXT defineItem("local:selectedTabId") lưu ở chrome.storage.local key "selectedTabId".
  // Ghi cả hai để popup hết nút Connect.
  await chrome.storage.local.set({
    "selectedTabId": tab.id,
    "local:selectedTabId": tab.id
  });
  return tab.id;
})()`

type target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func waitDebug(ctx context.Context, port int) error {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("debug port status %d", resp.StatusCode)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("chrome debug port %d khong san sang: %w", port, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func loadUnpacked(ctx context.Context, port int, extDir string) error {
	wsURL, err := browserDebuggerURL(ctx, port)
	if err != nil {
		return err
	}
	if _, err := cdpCall(ctx, wsURL, "Extensions.loadUnpacked", map[string]any{"path": extDir}); err != nil {
		return fmt.Errorf("khong nap duoc extension: %w", err)
	}
	return nil
}

func browserDebuggerURL(ctx context.Context, port int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var info struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("doc chrome version: %w", err)
	}
	if info.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("chrome khong co browser websocket")
	}
	return info.WebSocketDebuggerURL, nil
}

func cdpCall(ctx context.Context, wsURL, method string, params any) (json.RawMessage, error) {
	conn, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	payload, err := json.Marshal(map[string]any{
		"id":     1,
		"method": method,
		"params": params,
	})
	if err != nil {
		return nil, err
	}
	if err := conn.writeText(payload); err != nil {
		return nil, err
	}
	for {
		msg, err := conn.readText(ctx)
		if err != nil {
			return nil, err
		}
		var envelope cdpMessage
		if err := json.Unmarshal(msg, &envelope); err != nil || envelope.ID != 1 {
			continue
		}
		if envelope.Error != nil {
			return nil, fmt.Errorf("cdp: %s", envelope.Error.Message)
		}
		return envelope.Result, nil
	}
}

func selectBrowserMCPTab(ctx context.Context, port int) (int, error) {
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for {
		id, err := trySelectTab(ctx, port)
		if err == nil {
			return id, nil
		}
		last = err
		if time.Now().After(deadline) {
			return 0, last
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func trySelectTab(ctx context.Context, port int) (int, error) {
	targets, err := listTargets(ctx, port)
	if err != nil {
		return 0, err
	}
	var saw bool
	for _, t := range targets {
		if t.Type != "service_worker" || t.WebSocketDebuggerURL == "" {
			continue
		}
		saw = true
		name, err := evalString(ctx, t.WebSocketDebuggerURL, `chrome.runtime.getManifest().name`, false)
		if err != nil || name != browserMCPName {
			continue
		}
		raw, err := evalRaw(ctx, t.WebSocketDebuggerURL, selectTabJS, true)
		if err != nil {
			return 0, err
		}
		var tabID int
		if err := json.Unmarshal(raw, &tabID); err != nil {
			return 0, fmt.Errorf("tab id khong hop le: %s", raw)
		}
		return tabID, nil
	}
	if !saw {
		return 0, fmt.Errorf("chua thay service worker")
	}
	return 0, fmt.Errorf("chua thay extension %s", browserMCPName)
}

func listTargets(ctx context.Context, port int) ([]target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/list", port), nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var targets []target
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("doc debug targets: %w", err)
	}
	return targets, nil
}

type cdpMessage struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type evalResult struct {
	Result struct {
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails json.RawMessage `json:"exceptionDetails"`
}

func evalString(ctx context.Context, wsURL, expr string, await bool) (string, error) {
	raw, err := evalRaw(ctx, wsURL, expr, await)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("gia tri khong phai chuoi: %s", raw)
	}
	return s, nil
}

func evalRaw(ctx context.Context, wsURL, expr string, await bool) (json.RawMessage, error) {
	conn, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	payload, err := json.Marshal(map[string]any{
		"id":     1,
		"method": "Runtime.evaluate",
		"params": map[string]any{
			"expression":    expr,
			"awaitPromise":  await,
			"returnByValue": true,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := conn.writeText(payload); err != nil {
		return nil, err
	}
	for {
		msg, err := conn.readText(ctx)
		if err != nil {
			return nil, err
		}
		var envelope cdpMessage
		if err := json.Unmarshal(msg, &envelope); err != nil || envelope.ID != 1 {
			continue
		}
		if envelope.Error != nil {
			return nil, fmt.Errorf("cdp: %s", envelope.Error.Message)
		}
		var result evalResult
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			return nil, err
		}
		if len(result.ExceptionDetails) > 0 && string(result.ExceptionDetails) != "null" {
			return nil, fmt.Errorf("evaluate loi: %s", truncate(string(result.ExceptionDetails), 300))
		}
		if len(result.Result.Value) == 0 {
			return nil, fmt.Errorf("evaluate khong tra ve gia tri")
		}
		return result.Result.Value, nil
	}
}

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nOrigin: http://%s\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, u.Host, key)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		conn.Close()
		return nil, fmt.Errorf("websocket tu choi: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsConn{conn: conn, br: br}, nil
}

func (c *wsConn) Close() error { return c.conn.Close() }

func (c *wsConn) writeText(payload []byte) error {
	header := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n < 65536:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(n))
		header = append(header, size[:]...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

func (c *wsConn) readText(ctx context.Context) ([]byte, error) {
	var buf []byte
	for {
		if deadline, ok := ctx.Deadline(); ok {
			_ = c.conn.SetReadDeadline(deadline)
		}
		var hdr [2]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			return nil, err
		}
		fin := hdr[0]&0x80 != 0
		opcode := hdr[0] & 0x0f
		masked := hdr[1]&0x80 != 0
		length := uint64(hdr[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		var mask []byte
		if masked {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(c.br, mask); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x1:
			buf = append(buf[:0], payload...)
			if fin {
				return buf, nil
			}
		case 0x0:
			buf = append(buf, payload...)
			if fin {
				return buf, nil
			}
		case 0x8:
			return nil, fmt.Errorf("websocket dong")
		case 0x9:
			if err := c.writePong(payload); err != nil {
				return nil, err
			}
		}
	}
}

func (c *wsConn) writePong(payload []byte) error {
	if len(payload) >= 126 {
		return nil
	}
	frame := []byte{0x8A, 0x80 | byte(len(payload))}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}
