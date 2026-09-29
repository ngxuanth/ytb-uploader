package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultCallTimeout covers the extension's own waits: a click alone is
	// 3–6 s of mouse moves and DOM-settle waits.
	DefaultCallTimeout = 45 * time.Second
	// NavigationTimeout is a bit above the patched extension's 60 s cap.
	NavigationTimeout = 75 * time.Second
	// reconnectWait is how long a call waits for a dropped instance to
	// reconnect (the extension retries every second).
	reconnectWait = 15 * time.Second
)

// ErrOffline means the instance did not come back within reconnectWait.
var ErrOffline = errors.New("driver: extension instance is offline")

// Browser drives one Chrome profile.
type Browser struct {
	s  *Server
	id string
}

func (b *Browser) InstanceID() string { return b.id }

func (b *Browser) conn(ctx context.Context) (*Conn, error) {
	if c, ok := b.s.Conn(b.id); ok {
		return c, nil
	}
	wctx, cancel := context.WithTimeout(ctx, reconnectWait)
	defer cancel()
	c, err := b.s.WaitFor(wctx, func(h Hello) bool { return h.InstanceID == b.id })
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrOffline
	}
	return c, nil
}

// Call runs one extension message with a timeout. A call is never re-sent
// after a disconnect, since most of them (click, type) are not idempotent.
func (b *Browser) Call(ctx context.Context, timeout time.Duration, typ string, payload any, out any) error {
	c, err := b.conn(ctx)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if payload == nil {
		payload = struct{}{}
	}
	return c.Call(cctx, typ, payload, out)
}

type TabInfo struct {
	TabID int    `json:"tabId"`
	URL   string `json:"url"`
}

// OpenTab creates a tab, makes it the one tools act on, and waits for it to load.
func (b *Browser) OpenTab(ctx context.Context, url string) (*TabInfo, error) {
	var t TabInfo
	err := b.Call(ctx, NavigationTimeout, "openTab", map[string]any{"url": url}, &t)
	return &t, err
}

// SelectTab points tools at the first tab whose URL contains urlMatch.
func (b *Browser) SelectTab(ctx context.Context, urlMatch string) (*TabInfo, error) {
	var t TabInfo
	err := b.Call(ctx, DefaultCallTimeout, "selectTab", map[string]any{"urlMatch": urlMatch}, &t)
	return &t, err
}

// CloseTab closes the selected tab, which also aborts an upload running in it.
func (b *Browser) CloseTab(ctx context.Context) error {
	return b.Call(ctx, DefaultCallTimeout, "closeTab", nil, nil)
}

func (b *Browser) Navigate(ctx context.Context, url string) error {
	return b.Call(ctx, NavigationTimeout, "browser_navigate", map[string]any{"url": url}, nil)
}

func (b *Browser) URL(ctx context.Context) (string, error) {
	var u string
	err := b.Call(ctx, DefaultCallTimeout, "getUrl", nil, &u)
	return u, err
}

// Evaluate runs a JS expression in the page and decodes its (JSON-serialisable)
// value into out. Promises are awaited.
func (b *Browser) Evaluate(ctx context.Context, expression string, out any) error {
	var raw json.RawMessage
	if err := b.Call(ctx, DefaultCallTimeout, "browser_evaluate", map[string]any{"expression": expression}, &raw); err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ClickSelector does a real (CDP mouse) click on the element.
func (b *Browser) ClickSelector(ctx context.Context, selector string) error {
	return b.Call(ctx, DefaultCallTimeout, "browser_click_selector", map[string]any{"selector": selector}, nil)
}

// TypeSelector clears the field and types text with real key events.
func (b *Browser) TypeSelector(ctx context.Context, selector, text string, submit bool) error {
	return b.Call(ctx, DefaultCallTimeout+time.Duration(len(text))*20*time.Millisecond,
		"browser_type_selector", map[string]any{"selector": selector, "text": text, "submit": submit}, nil)
}

// Scroll centres the element matching selector in the viewport, so a sticky
// footer cannot cover it.
func (b *Browser) Scroll(ctx context.Context, selector string) error {
	return b.Call(ctx, DefaultCallTimeout, "browser_scroll", map[string]any{"selector": selector}, nil)
}

func (b *Browser) PressKey(ctx context.Context, key string) error {
	return b.Call(ctx, DefaultCallTimeout, "browser_press_key", map[string]any{"key": key}, nil)
}

// UploadFile attaches a local file to the input matching selector (searched
// through shadow roots and same-origin iframes). Chrome reads the file from
// disk, so the path must exist on this machine.
func (b *Browser) UploadFile(ctx context.Context, selector, path string) error {
	return b.Call(ctx, DefaultCallTimeout, "browser_upload_file", map[string]any{"selector": selector, "filePath": path}, nil)
}

// Screenshot returns a PNG of the viewport.
func (b *Browser) Screenshot(ctx context.Context) ([]byte, error) {
	var s string
	if err := b.Call(ctx, DefaultCallTimeout, "browser_screenshot", nil, &s); err != nil {
		return nil, err
	}
	png, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("driver: decode screenshot: %w", err)
	}
	return png, nil
}

// Snapshot returns the aria snapshot text (useful when debugging selectors).
func (b *Browser) Snapshot(ctx context.Context) (string, error) {
	var s string
	err := b.Call(ctx, DefaultCallTimeout, "browser_snapshot", nil, &s)
	return s, err
}
