package studio

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestEmbeddedScriptsParse runs `node --check` on every embedded page script
// so a JavaScript syntax error fails the build instead of a live upload. It is
// skipped when node is not installed, so it never blocks a Go-only checkout.
func TestEmbeddedScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS syntax check")
	}
	entries, err := scriptFS.ReadDir("js")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded scripts found under js/")
	}
	dir := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			src, err := scriptFS.ReadFile("js/" + name)
			if err != nil {
				t.Fatal(err)
			}
			f := filepath.Join(dir, name)
			if err := os.WriteFile(f, src, 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
				t.Fatalf("node --check %s failed:\n%s", name, out)
			}
		})
	}
}
