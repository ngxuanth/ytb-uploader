package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverProfiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ngxuanth", "isophtalic", "notes", "Default"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"isophtalic", "ngxuanth", "Default"} {
		if err := os.WriteFile(filepath.Join(dir, name, "Preferences"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := discoverProfiles(dir)
	if len(got) != 2 {
		t.Fatalf("len = %d, muon 2: %#v", len(got), got)
	}
	if got[0].Name != "isophtalic" || got[1].Name != "ngxuanth" {
		t.Fatalf("thu tu = %#v", got)
	}
	if got[0].Dir != filepath.Join(dir, "isophtalic") {
		t.Fatalf("dir = %s", got[0].Dir)
	}
}
