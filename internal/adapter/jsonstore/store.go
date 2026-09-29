// Package jsonstore keeps the server's tasks in one JSON file.
package jsonstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// Store is a taskservice.Repository backed by a JSON file.
type Store struct{ Path string }

func (s *Store) Load() ([]*task.Task, error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []*task.Task
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// Save writes a temporary file and renames it over the old one, so a crash
// never leaves a half-written file.
func (s *Store) Save(tasks []*task.Task) error {
	sort.Slice(tasks, func(a, b int) bool { return tasks[a].CreatedAt.Before(tasks[b].CreatedAt) })
	raw, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}
