package taskservice

import (
	"log"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/domain/task"
)

// UploadInput is POST /uploads.
type UploadInput struct {
	Profile     string `json:"profile"`
	Channel     string `json:"channel"`
	Video       string `json:"video"`
	Thumbnail   string `json:"thumbnail,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
}

// Upload publishes an upload to its profile's queue. It is assigned as soon
// as the profile is free and a launcher serves it, which may be right now.
func (s *Service) Upload(in UploadInput) (TaskView, error) {
	in.Profile = strings.TrimSpace(in.Profile)
	in.Channel = strings.TrimSpace(in.Channel)
	in.Video = strings.TrimSpace(in.Video)
	if in.Profile == "" || in.Video == "" {
		return TaskView{}, invalid("profile and video are required")
	}
	if in.Visibility == "" {
		in.Visibility = "private"
	}
	switch contract.Visibility(in.Visibility) {
	case contract.VisibilityPublic, contract.VisibilityUnlisted, contract.VisibilityPrivate:
	default:
		return TaskView{}, invalid("visibility must be public, unlisted or private")
	}
	if in.Title == "" {
		in.Title = strings.TrimSuffix(filepath.Base(in.Video), filepath.Ext(in.Video))
	}
	if in.Profile != filepath.Base(in.Profile) || in.Profile == "." || in.Profile == ".." {
		return TaskView{}, invalid("invalid profile name")
	}
	known, err := s.profiles.Names()
	if err != nil {
		return TaskView{}, err
	}
	if !slices.Contains(known, in.Profile) {
		return TaskView{}, &Error{Kind: Invalid, Msg: "unknown profile", Known: known}
	}
	t, err := s.newUpload(in)
	if err != nil {
		return TaskView{}, invalid(err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.add(t)
	task.Enqueue(s.all(), t, false)
	t.AddEvent(task.Event{Event: "queued"})
	log.Printf("queued %s profile %s channel %s", t.ID(), in.Profile, in.Channel)
	s.dispatchLocked()
	return s.viewLocked(t, true), nil
}

func (s *Service) newUpload(in UploadInput) (*task.Task, error) {
	fi, err := s.files.Inspect(in.Video)
	if err != nil {
		return nil, err
	}
	ext := filepath.Ext(in.Video)
	now := time.Now()
	t := &task.Task{
		Token: randHex(18), Sum: fi.SHA256, Ext: ext, Size: fi.Size,
		VideoPath: in.Video, VideoName: "video" + ext,
		Spec: task.Spec{
			Kind: task.KindUpload, TaskID: "srv-" + randHex(4), Attempt: 1,
			FilePath: "video" + ext, ChannelID: in.Channel, ProfileDirectory: in.Profile,
			Metadata: &contract.Metadata{
				Title: in.Title, Description: in.Description, Visibility: contract.Visibility(in.Visibility),
			},
		},
		Status: contract.StatusQueued, CreatedAt: now, UpdatedAt: now,
	}
	if in.Thumbnail != "" {
		if err := s.files.Exists(in.Thumbnail); err != nil {
			return nil, err
		}
		t.ThumbPath = in.Thumbnail
		t.ThumbName = "thumbnail" + filepath.Ext(in.Thumbnail)
		t.Spec.ThumbnailPath = t.ThumbName
	}
	return t, nil
}

// Profiles lists the Chrome profiles an upload may name.
func (s *Service) Profiles() ([]string, error) { return s.profiles.Names() }

// TaskFilter narrows ListTasks.
type TaskFilter struct {
	Statuses []contract.Status
	Profile  string
}

// ListTasks is every task matching f, newest first, without timelines.
func (s *Service) ListTasks(f TaskFilter) []TaskView {
	s.mu.Lock()
	out := []TaskView{}
	for _, t := range s.byID {
		if len(f.Statuses) > 0 && !t.Status.In(f.Statuses) {
			continue
		}
		if f.Profile != "" && t.Profile() != f.Profile {
			continue
		}
		out = append(out, s.viewLocked(t, false))
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// Task is one task with its timeline.
func (s *Service) Task(id string) (TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	if t == nil {
		return TaskView{}, notFound("no such task")
	}
	return s.viewLocked(t, true), nil
}

// File is the local path of the task's video or thumbnail named name, as
// launchers download it.
func (s *Service) File(id, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	switch {
	case t == nil:
	case name == t.VideoName && t.VideoPath != "":
		return t.VideoPath, nil
	case name == t.ThumbName && t.ThumbPath != "":
		return t.ThumbPath, nil
	}
	return "", notFound("no such file")
}
