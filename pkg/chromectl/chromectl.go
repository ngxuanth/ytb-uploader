// Package chromectl opens the upload Chrome with remote debugging, finds a
// profile's Browser MCP extension, installs it when missing and points it at
// the session's bmcp port. It is what the "chrome" MCP tools call.
//
// Profiles are discovered in UserDataDir. Each running profile is opened from
// its own user-data-dir and debugging port (see Isolated), so one session
// exiting cannot take down the Chrome another profile is using.
package chromectl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/cdp"
)

// ExtensionName is the manifest name of the patched Browser MCP extension.
const ExtensionName = "Browser MCP Local"

type Controller struct {
	Bin          string // google-chrome
	UserDataDir  string // absolute
	DebugPort    int
	ExtensionDir string // unpacked extension (manifest.json)
}

type Profile struct {
	Directory string `json:"directory"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
}

// Profiles lists the profiles Chrome knows about in the user-data-dir.
func (c *Controller) Profiles() ([]Profile, error) {
	b, err := os.ReadFile(filepath.Join(c.UserDataDir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("read Local State: %w", err)
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name     string `json:"name"`
				UserName string `json:"user_name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(b, &ls); err != nil {
		return nil, fmt.Errorf("parse Local State: %w", err)
	}
	var out []Profile
	for dir, p := range ls.Profile.InfoCache {
		out = append(out, Profile{Directory: dir, Name: p.Name, Email: p.UserName})
	}
	return out, nil
}

func (c *Controller) hasProfile(dir string) error {
	ps, err := c.Profiles()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Directory == dir {
			return nil
		}
	}
	return fmt.Errorf("profile %q not found in %s", dir, c.UserDataDir)
}

// OpenResult says what Open had to do.
type OpenResult struct {
	Restarted bool   `json:"restarted"`
	Started   bool   `json:"started"`
	Message   string `json:"message"`
}

// Open makes sure Chrome runs with the debugging port and that the profile
// has a window (which wakes its extension).
func (c *Controller) Open(ctx context.Context, profileDir string) (*OpenResult, error) {
	if err := c.hasProfile(profileDir); err != nil {
		return nil, err
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &OpenResult{}
	if _, err := cdp.BrowserWSURL(ctx, c.DebugPort); err != nil {
		pids := c.chromePIDs()
		if len(pids) > 0 {
			// Chrome runs on this user-data-dir without the debugging
			// port: it has to be restarted to be controllable.
			if err := stopPIDs(ctx, pids); err != nil {
				return nil, err
			}
			res.Restarted = true
		}
		if err := c.start(profileDir, "about:blank"); err != nil {
			return nil, err
		}
		res.Started = true
		if err := c.waitDebug(ctx, 30*time.Second); err != nil {
			return nil, err
		}
	} else if err := c.start(profileDir, "about:blank"); err != nil {
		// Already debuggable: this hands the profile to the running Chrome.
		return nil, err
	}
	switch {
	case res.Restarted:
		res.Message = "Chrome was running without remote debugging and was restarted"
	case res.Started:
		res.Message = "Chrome started"
	default:
		res.Message = "Chrome already running; profile window opened"
	}
	return res, nil
}

func (c *Controller) start(profileDir, url string) error {
	args := []string{
		"--user-data-dir=" + c.UserDataDir,
		"--profile-directory=" + profileDir,
		"--remote-debugging-port=" + strconv.Itoa(c.DebugPort),
		"--remote-allow-origins=http://127.0.0.1",
		"--no-first-run", "--no-default-browser-check",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
	}
	args = append(args, chromePlatformArgs()...)
	args = append(args, url)
	cmd := exec.Command(c.Bin, args...)
	// Chrome must outlive the session that opened it.
	cmd.SysProcAttr = chromeSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	// The next attempt on this profile reuses this Chrome (see Isolated).
	_ = os.WriteFile(filepath.Join(c.UserDataDir, debugPortFile), []byte(strconv.Itoa(c.DebugPort)), 0o600)
	return nil
}

// debugPortFile records, in a user-data-dir, the debugging port its Chrome
// was started with.
const debugPortFile = ".debug-port"

// runningDebugPort is the debugging port of the Chrome already running on
// this user-data-dir, or 0 if none answers.
func (c *Controller) runningDebugPort(ctx context.Context) int {
	b, err := os.ReadFile(filepath.Join(c.UserDataDir, debugPortFile))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || port <= 0 || len(c.chromePIDs()) == 0 {
		return 0
	}
	if _, err := cdp.BrowserWSURL(ctx, port); err != nil {
		return 0
	}
	return port
}

func (c *Controller) waitDebug(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if _, err := cdp.BrowserWSURL(ctx, c.DebugPort); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("chrome debugging port %d not ready: %w", c.DebugPort, err)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

// contextOf finds the browserContextId of a profile by opening a marker tab
// in it (a profile directory is not visible over CDP any other way).
func (c *Controller) contextOf(ctx context.Context, conn *cdp.Conn, profileDir string) (string, error) {
	nonce := randHex(8)
	// about:blank#fragment opens as a new-tab page; a data: URL keeps its text.
	marker := "data:text/html,uploader-" + nonce
	if err := c.start(profileDir, marker); err != nil {
		return "", err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ts, err := conn.Targets(ctx)
		if err != nil {
			return "", err
		}
		for _, t := range ts {
			if t.Type == "page" && strings.Contains(t.URL, nonce) {
				_ = conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.TargetID}, nil)
				return t.BrowserContextID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("could not locate profile %q in the running Chrome", profileDir)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return "", err
		}
	}
}

// extensionWorker finds the service worker of the extension loaded from
// ExtensionDir in a profile. A copy with the same name loaded from another
// directory (an old checkout) is not it: it may lack tools bmcp calls, which
// then hang until the socket times out.
func (c *Controller) extensionWorker(ctx context.Context, conn *cdp.Conn, contextID string) (session string, extID string, err error) {
	want := unpackedID(c.ExtensionDir)
	ws, err := c.namedWorkers(ctx, conn, contextID)
	if err != nil {
		return "", "", err
	}
	found := false
	for _, w := range ws {
		if !found && (want == "" || w.id == want) {
			session, extID, found = w.session, w.id, true
			continue
		}
		conn.Detach(ctx, w.session)
	}
	if !found {
		return "", "", errNoWorker
	}
	return session, extID, nil
}

type worker struct{ session, id string }

// namedWorkers attaches to every running service worker in the profile whose
// extension is named ExtensionName. The caller detaches them.
func (c *Controller) namedWorkers(ctx context.Context, conn *cdp.Conn, contextID string) ([]worker, error) {
	ts, err := conn.Targets(ctx)
	if err != nil {
		return nil, err
	}
	var out []worker
	for _, t := range ts {
		if t.Type != "service_worker" || t.BrowserContextID != contextID || !strings.HasPrefix(t.URL, "chrome-extension://") {
			continue
		}
		s, err := conn.Attach(ctx, t.TargetID)
		if err != nil {
			continue
		}
		var name string
		if conn.Eval(ctx, s, "chrome.runtime.getManifest().name", &name) == nil && name == ExtensionName {
			id := strings.TrimPrefix(t.URL, "chrome-extension://")
			if i := strings.Index(id, "/"); i >= 0 {
				id = id[:i]
			}
			out = append(out, worker{s, id})
			continue
		}
		conn.Detach(ctx, s)
	}
	return out, nil
}

// retireStale removes copies of the extension loaded from other directories,
// so only one of them connects to the session's bmcp port. When Chrome
// refuses to uninstall one, its port is pointed at 1, which it never reaches.
func (c *Controller) retireStale(ctx context.Context, conn *cdp.Conn, contextID, keep string) []string {
	ws, err := c.namedWorkers(ctx, conn, contextID)
	if err != nil {
		return nil
	}
	var retired []string
	for _, w := range ws {
		if w.id != keep {
			if conn.Call(ctx, "", "Extensions.uninstall", map[string]any{"id": w.id}, nil) != nil {
				_ = conn.Eval(ctx, w.session, "chrome.storage.local.set({wsPort:1}).then(()=>true)", nil)
			}
			retired = append(retired, w.id)
		}
		conn.Detach(ctx, w.session)
	}
	return retired
}

// codeHash fingerprints the extension's scripts on disk, so Setup can tell
// when the running copy is older than ExtensionDir.
func (c *Controller) codeHash() string {
	h := sha256.New()
	for _, name := range []string{"manifest.json", "background.js", "content-scripts/content.js"} {
		b, err := os.ReadFile(filepath.Join(c.ExtensionDir, name))
		if err != nil {
			return ""
		}
		h.Write([]byte(name))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// unpackedID is the id Chrome gives an unpacked extension without a "key":
// the first 128 bits of SHA-256 of its absolute path, as letters a-p.
// pathIDBytes matches the bytes Chrome hashes (UTF-8 on Linux, UTF-16LE on Windows).
func unpackedID(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return extensionID(pathIDBytes(abs))
}

func extensionID(b []byte) string {
	sum := sha256.Sum256(b)
	id := make([]byte, 32)
	for i, c := range sum[:16] {
		id[2*i] = 'a' + c>>4
		id[2*i+1] = 'a' + c&0xf
	}
	return string(id)
}

var errNoWorker = errors.New("extension service worker not found")

// SetupResult reports the extension state after Setup.
type SetupResult struct {
	ExtensionID string `json:"extension_id"`
	Installed   bool   `json:"installed"` // loaded by this call
	Reloaded    bool   `json:"reloaded"`  // had to reload to switch port or load new code
	// Retired lists copies of the extension from other directories that were
	// removed (or cut off from bmcp) so they cannot answer in its place.
	Retired []string `json:"retired,omitempty"`
	WSPort  int      `json:"ws_port"`
	TabID   int      `json:"tab_id"`
	Message string   `json:"message"`
}

// Setup installs the extension in the profile if needed, points it at port
// and opens (and selects) a tab on url.
func (c *Controller) Setup(ctx context.Context, profileDir string, port int, url string) (*SetupResult, error) {
	conn, err := cdp.Dial(ctx, c.DebugPort)
	if err != nil {
		return nil, fmt.Errorf("chrome is not open with remote debugging (call chrome_open first): %w", err)
	}
	defer conn.Close()
	cid, err := c.contextOf(ctx, conn, profileDir)
	if err != nil {
		return nil, err
	}

	res := &SetupResult{WSPort: port}
	session, extID, err := c.waitWorker(ctx, conn, cid, 5*time.Second)
	if errors.Is(err, errNoWorker) {
		// Branded Chrome ignores --load-extension; load it over CDP.
		var r struct {
			ID string `json:"id"`
		}
		if err := conn.Call(ctx, "", "Extensions.loadUnpacked", map[string]any{"path": c.ExtensionDir}, &r); err != nil {
			return nil, fmt.Errorf("install extension: %w", err)
		}
		res.Installed = true
		session, extID, err = c.waitWorker(ctx, conn, cid, 15*time.Second)
		if errors.Is(err, errNoWorker) && r.ID != "" {
			// A new profile may not start the worker until an event.
			extID = r.ID
			session, err = c.wakeWorker(ctx, conn, cid, extID)
		}
		if err != nil {
			return nil, fmt.Errorf("extension loaded (%s) but its worker did not start in this profile: %w", r.ID, err)
		}
	} else if err != nil {
		return nil, err
	}
	res.ExtensionID = extID
	if stale := c.retireStale(ctx, conn, cid, extID); len(stale) > 0 {
		res.Retired = stale
	}

	var tab struct {
		TabID int `json:"tabId"`
	}
	expr := fmt.Sprintf(`(async()=>{
await chrome.storage.local.set({wsPort:%d});
const t=await chrome.tabs.create({url:%q,active:true});
await chrome.storage.local.set({selectedTabId:t.id});
for (const o of await chrome.tabs.query({})) {
  if (o.id!==t.id && /^(https:\/\/studio\.youtube\.com\/|about:blank|chrome:\/\/newtab)/.test(o.url||o.pendingUrl||"")) { try { await chrome.tabs.remove(o.id) } catch {} }
}
return {tabId:t.id};})()`, port, url)
	if err := conn.Eval(ctx, session, expr, &tab); err != nil {
		return nil, fmt.Errorf("configure extension: %w", err)
	}
	res.TabID = tab.TabID

	// A worker that was already running may keep an old socket alive, or run
	// code older than ExtensionDir (Chrome does not watch unpacked files);
	// reloading the extension fixes both.
	hash := c.codeHash()
	var loaded string
	_ = conn.Eval(ctx, session, `chrome.storage.local.get("codeHash").then(s=>s.codeHash||"")`, &loaded)
	stale := hash != "" && loaded != hash
	if stale {
		_ = conn.Eval(ctx, session, fmt.Sprintf("chrome.storage.local.set({codeHash:%q}).then(()=>true)", hash), nil)
	}
	// An extension this call just loaded runs the current code already;
	// reloading it right away can leave a new profile without a worker.
	if res.Installed {
		stale = false
	}
	if stale || !c.usesPort(ctx, conn, session, port, 5*time.Second) {
		_ = conn.Eval(ctx, session, "setTimeout(()=>chrome.runtime.reload(),50),true", nil)
		conn.Detach(ctx, session)
		if err := sleep(ctx, 2*time.Second); err != nil {
			return nil, err
		}
		session, _, err = c.waitWorker(ctx, conn, cid, 15*time.Second)
		if errors.Is(err, errNoWorker) {
			// An MV3 worker only starts on an event: open one of the
			// extension's pages to wake it, then look again.
			session, err = c.wakeWorker(ctx, conn, cid, extID)
		}
		if err != nil {
			return nil, fmt.Errorf("extension did not come back after reload: %w", err)
		}
		if err := conn.Eval(ctx, session, fmt.Sprintf("chrome.storage.local.set({selectedTabId:%d}).then(()=>true)", tab.TabID), nil); err != nil {
			return nil, err
		}
		if !c.usesPort(ctx, conn, session, port, 5*time.Second) {
			return nil, fmt.Errorf("extension still does not use port %d after reload", port)
		}
		res.Reloaded = true
	}
	res.Message = fmt.Sprintf("extension points at port %d and drives tab %d", port, tab.TabID)
	return res, nil
}

// wakeWorker opens the extension's popup page in the profile, which starts
// its service worker, waits for the worker and closes the page again.
func (c *Controller) wakeWorker(ctx context.Context, conn *cdp.Conn, cid, extID string) (string, error) {
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := conn.Call(ctx, "", "Target.createTarget", map[string]any{
		"url": "chrome-extension://" + extID + "/popup.html", "browserContextId": cid, "background": true,
	}, &t); err != nil {
		return "", err
	}
	defer func() { _ = conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.TargetID}, nil) }()
	session, _, err := c.waitWorker(ctx, conn, cid, 10*time.Second)
	return session, err
}

// usesPort waits until the extension's connect loop targets port.
func (c *Controller) usesPort(ctx context.Context, conn *cdp.Conn, session string, port int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		var got int
		if conn.Eval(ctx, session, "globalThis.__bmcpPort||0", &got) == nil && got == port {
			return true
		}
		if time.Now().After(deadline) || sleep(ctx, 500*time.Millisecond) != nil {
			return false
		}
	}
}

func (c *Controller) waitWorker(ctx context.Context, conn *cdp.Conn, cid string, d time.Duration) (string, string, error) {
	deadline := time.Now().Add(d)
	for {
		s, id, err := c.extensionWorker(ctx, conn, cid)
		if err == nil || !errors.Is(err, errNoWorker) || time.Now().After(deadline) {
			return s, id, err
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return "", "", err
		}
	}
}

// Status is what the extension reports about its socket.
type Status struct {
	Installed     bool   `json:"installed"`
	Connected     bool   `json:"connected"`
	ConnectedPort int    `json:"connected_port,omitempty"`
	WSPort        int    `json:"ws_port,omitempty"`
	SelectedTab   int    `json:"selected_tab_id,omitempty"`
	SelectedURL   string `json:"selected_tab_url,omitempty"`
	Message       string `json:"message"`
}

func (c *Controller) Status(ctx context.Context, profileDir string) (*Status, error) {
	conn, err := cdp.Dial(ctx, c.DebugPort)
	if err != nil {
		return nil, fmt.Errorf("chrome is not open with remote debugging: %w", err)
	}
	defer conn.Close()
	cid, err := c.contextOf(ctx, conn, profileDir)
	if err != nil {
		return nil, err
	}
	session, _, err := c.waitWorker(ctx, conn, cid, 3*time.Second)
	if errors.Is(err, errNoWorker) {
		return &Status{Message: "extension not installed or not running in this profile (call extension_setup)"}, nil
	} else if err != nil {
		return nil, err
	}
	st := &Status{Installed: true}
	if err := conn.Eval(ctx, session, `(async()=>{
const s=await chrome.storage.local.get(["wsPort","selectedTabId"]);
let url="";try{if(s.selectedTabId)url=(await chrome.tabs.get(s.selectedTabId)).url||""}catch{}
return {connected:globalThis.__bmcpConnected===true,connected_port:globalThis.__bmcpPort||0,ws_port:s.wsPort||0,selected_tab_id:s.selectedTabId||0,selected_tab_url:url};})()`, st); err != nil {
		return nil, err
	}
	switch {
	case st.Connected:
		st.Message = fmt.Sprintf("connected to bmcp on port %d", st.ConnectedPort)
	default:
		st.Message = "extension is not connected to bmcp yet (it retries every second)"
	}
	return st, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
