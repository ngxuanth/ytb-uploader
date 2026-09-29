package chrome

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsolatedGivesEachProfileItsOwnDirectory(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "chrome-profile")
	if err := os.MkdirAll(filepath.Join(src, "isophtalic"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"profile": map[string]any{
			"info_cache":     map[string]any{"isophtalic": map[string]any{"name": "Person 1"}},
			"last_used":      "Default",
			"profiles_order": []any{"isophtalic"},
		},
	}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(src, "Local State"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Controller{UserDataDir: src, DebugPort: 9222}
	iso, err := c.Isolated(context.Background(), "isophtalic", 9333)
	if err != nil {
		t.Fatal(err)
	}
	if iso.DebugPort != 9333 {
		t.Fatalf("port = %d", iso.DebugPort)
	}
	if iso.UserDataDir == src {
		t.Fatal("isolated controller still uses the shared user-data-dir")
	}
	if _, err := os.Stat(filepath.Join(iso.UserDataDir, "isophtalic")); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	raw, err := os.ReadFile(filepath.Join(iso.UserDataDir, "Local State"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	cache := got["profile"].(map[string]any)["info_cache"].(map[string]any)
	if _, ok := cache["isophtalic"]; !ok || len(cache) != 1 {
		t.Fatalf("info_cache = %#v", cache)
	}
	if c.UserDataDir != src || c.DebugPort != 9222 {
		t.Fatal("source controller was modified")
	}
	again, err := c.Isolated(context.Background(), "isophtalic", 9444)
	if err != nil {
		t.Fatal(err)
	}
	if again.UserDataDir != iso.UserDataDir || again.DebugPort != 9444 {
		t.Fatalf("second isolate = %s port %d", again.UserDataDir, again.DebugPort)
	}
}

func TestEnsureLinkMovesAReplacedFolderAside(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "profile", "p1")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "ports", "p1")
	// What Chrome leaves behind: a real profile folder where the link was.
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "Preferences"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLink(link, target); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || !samePath(got, target) {
		t.Fatalf("link: %q %v", got, err)
	}
	aside, _ := filepath.Glob(link + ".broken-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
	if _, err := os.Stat(filepath.Join(aside[0], "Preferences")); err != nil {
		t.Fatal(err)
	}
	// A correct link is left alone.
	if err := ensureLink(link, target); err != nil {
		t.Fatal(err)
	}
	if again, _ := filepath.Glob(link + ".broken-*"); len(again) != 1 {
		t.Fatalf("second call moved again: %v", again)
	}
}
