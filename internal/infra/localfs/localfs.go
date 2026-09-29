// Package localfs reads what the server needs from the local disk: the
// Chrome profiles an upload may name, and the video files it points at.
package localfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/taskservice"
)

// Profiles lists the Chrome profile folders in Dir, which is a
// user-data-dir: only folders with a Preferences file are profiles, the rest
// (Safe Browsing, WidevineCdm, ...) are Chrome's own.
type Profiles struct{ Dir string }

func (p Profiles) Names() ([]string, error) {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.Dir, name, "Preferences")); err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Files hashes and stats local files.
type Files struct{}

func (Files) Inspect(path string) (taskservice.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return taskservice.FileInfo{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return taskservice.FileInfo{}, err
	}
	return taskservice.FileInfo{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func (Files) Exists(path string) error {
	_, err := os.Stat(path)
	return err
}
