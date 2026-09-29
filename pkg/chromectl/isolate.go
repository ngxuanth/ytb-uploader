package chromectl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Isolated returns a controller that opens this profile in its own Chrome
// process on port. Chrome allows one process per user-data-dir, so two
// profiles can only have two debugging ports when each has its own directory.
// The profile folder stays where it is; the new directory links to it, so the
// Google login is the same one.
func (c *Controller) Isolated(ctx context.Context, profile string, port int) (*Controller, error) {
	if port <= 0 {
		return nil, fmt.Errorf("debug port is required")
	}
	if profile == "" || profile != filepath.Base(profile) || strings.Contains(profile, "..") {
		return nil, fmt.Errorf("invalid profile %q", profile)
	}
	if err := c.hasProfile(profile); err != nil {
		return nil, err
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// The shared directory's Chrome holds every profile's files. It has to
	// exit before a per-profile Chrome can open the same folder.
	if err := stopPIDs(ctx, c.chromePIDs()); err != nil {
		return nil, err
	}
	dir, err := c.isolate(profile)
	if err != nil {
		return nil, err
	}
	next := *c
	next.UserDataDir = dir
	next.DebugPort = port
	return &next, nil
}

// isolate builds <repo>/data/chrome-ports/<profile> with a link to the real
// profile folder and a Local State that names only that profile.
func (c *Controller) isolate(profile string) (string, error) {
	root := filepath.Join(filepath.Dir(c.UserDataDir), "data", "chrome-ports", profile)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	src := filepath.Join(c.UserDataDir, profile)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return "", fmt.Errorf("profile folder %s: %w", src, err)
	}
	if err := ensureLink(filepath.Join(root, profile), src); err != nil {
		return "", fmt.Errorf("link profile: %w", err)
	}
	if err := seedLocalState(c.UserDataDir, root, profile); err != nil {
		return "", fmt.Errorf("local state: %w", err)
	}
	return root, nil
}

// ensureLink makes link a link to target. The isolated directory is ours, so
// anything else found there is moved aside rather than refused: Chrome
// replaces the link with a fresh profile folder when it opens a profile that
// another Chrome holds, and every later task of the profile would fail.
func ensureLink(link, target string) error {
	target, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	_, lerr := os.Lstat(link)
	if lerr == nil {
		got, rerr := os.Readlink(link)
		if rerr == nil && samePath(strings.TrimPrefix(got, `\??\`), target) {
			return nil
		}
		aside := link + ".broken-" + time.Now().Format("20060102-150405")
		if err := os.Rename(link, aside); err != nil {
			return fmt.Errorf("%s is not a link to the profile and cannot be moved aside: %w", link, err)
		}
	} else if !os.IsNotExist(lerr) {
		return lerr
	}
	return linkDir(link, target)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	return strings.EqualFold(a, b)
}

func seedLocalState(srcRoot, dstRoot, profile string) error {
	dst := filepath.Join(dstRoot, "Local State")
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	b, err := os.ReadFile(filepath.Join(srcRoot, "Local State"))
	if err != nil {
		return fmt.Errorf("read Local State: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse Local State: %w", err)
	}
	raw, _ := doc["profile"].(map[string]any)
	if raw == nil {
		return fmt.Errorf("Local State has no profile section")
	}
	cache, _ := raw["info_cache"].(map[string]any)
	entry, ok := cache[profile]
	if !ok {
		return fmt.Errorf("profile %q is not in Local State", profile)
	}
	raw["info_cache"] = map[string]any{profile: entry}
	raw["profiles_order"] = []any{profile}
	raw["last_used"] = profile
	if active, ok := raw["last_active_profiles"].([]any); ok && len(active) > 0 {
		raw["last_active_profiles"] = []any{profile}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, out, 0o600)
}
