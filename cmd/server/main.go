// Command server is a stand-in for the upload server. It serves task_mcp and
// report_mcp plus a small REST API (see api.go). An upload starts only when
// POST /uploads names a profile and a video; channel is optional. The connected agent then
// receives that task over WebSocket. Every task_mcp / report_mcp call updates
// the task, and tasks are saved to a JSON file.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/pkg/wire"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	profiles := flag.String("profiles", "profile", "directory whose subfolders are Chrome profiles")
	data := flag.String("data", "data/server/tasks.json", "JSON file the tasks are saved to")
	flag.Parse()
	absProfiles, err := filepath.Abs(*profiles)
	if err != nil {
		log.Fatal(err)
	}
	absData, err := filepath.Abs(*data)
	if err != nil {
		log.Fatal(err)
	}
	st, err := newState("http://"+*addr, absProfiles, absData)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on http://%s  ws://%s/ws", *addr, *addr)
	log.Printf("profiles are folders in %s; tasks are saved to %s", absProfiles, absData)
	log.Fatal(st.app().Listen(*addr))
}

type uploadIn struct {
	Profile     string `json:"profile"`
	Channel     string `json:"channel"`
	Video       string `json:"video"`
	Thumbnail   string `json:"thumbnail,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
}

// agentInfo is what the connected launcher announced.
type agentInfo struct {
	AgentID     string              `json:"agent_id"`
	Version     string              `json:"version"`
	Profiles    []wire.ProfileState `json:"profiles"`
	ConnectedAt time.Time           `json:"connected_at"`
	LastSeen    time.Time           `json:"last_seen"`
}

type state struct {
	base        string
	profilesDir string
	store       *store

	mu      sync.Mutex
	conn    *websocket.Conn
	agent   *agentInfo
	byToken map[string]*job
	byID    map[string]*job
}

func newState(base, profilesDir, dataFile string) (*state, error) {
	st := &state{
		base:        strings.TrimRight(base, "/"),
		profilesDir: profilesDir,
		store:       &store{path: dataFile},
		byToken:     map[string]*job{},
		byID:        map[string]*job{},
	}
	jobs, err := st.store.load()
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		st.byID[j.Claim.TaskID] = j
		st.byToken[j.Token] = j
	}
	return st, nil
}

// saveLocked writes every job to the store. The caller holds st.mu.
func (st *state) saveLocked() {
	jobs := make([]*job, 0, len(st.byID))
	for _, j := range st.byID {
		jobs = append(jobs, j)
	}
	if err := st.store.save(jobs); err != nil {
		log.Printf("save tasks: %v", err)
	}
}

// sendLocked writes one message to the agent. The caller holds st.mu.
func (st *state) sendLocked(typ string, data any) error {
	if st.conn == nil {
		return errors.New("agent is not connected")
	}
	msg, err := wire.NewEnvelope(typ, data)
	if err != nil {
		return err
	}
	return st.conn.WriteJSON(msg)
}

func (st *state) app() *fiber.App {
	app := fiber.New()
	taskH := adaptor.HTTPHandler(taskmcp.NewTaskHandler(st))
	reportH := adaptor.HTTPHandler(taskmcp.NewReportHandler(st))
	app.All("/task/mcp", taskH)
	app.All("/task/mcp/*", taskH)
	app.All("/report/mcp", reportH)
	app.All("/report/mcp/*", reportH)
	app.Get("/profiles", st.profilesHandler)
	app.Get("/agent", st.agentHandler)
	app.Post("/uploads", st.upload)
	app.Get("/tasks", st.listTasks)
	app.Get("/tasks/:id", st.getTask)
	app.Post("/tasks/:id/retry", st.retryTask)
	app.Post("/tasks/:id/cancel", st.cancelTask)
	app.Get("/files/:id/:name", st.file)
	app.Get("/ws", websocket.New(st.ws))
	return app
}

func (st *state) profilesHandler(c fiber.Ctx) error {
	names, err := st.profileNames()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"profiles": names})
}

func (st *state) upload(c fiber.Ctx) error {
	var in uploadIn
	if err := c.Bind().JSON(&in); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	in.Profile = strings.TrimSpace(in.Profile)
	in.Channel = strings.TrimSpace(in.Channel)
	in.Video = strings.TrimSpace(in.Video)
	if in.Profile == "" || in.Video == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "profile and video are required"})
	}
	if in.Visibility == "" {
		in.Visibility = "private"
	}
	switch wire.Visibility(in.Visibility) {
	case wire.VisibilityPublic, wire.VisibilityUnlisted, wire.VisibilityPrivate:
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "visibility must be public, unlisted or private"})
	}
	if in.Title == "" {
		in.Title = strings.TrimSuffix(filepath.Base(in.Video), filepath.Ext(in.Video))
	}
	if !validProfileName(in.Profile) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile name"})
	}
	known, err := st.profileNames()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !contains(known, in.Profile) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown profile", "profiles": known})
	}

	st.mu.Lock()
	connected := st.conn != nil
	st.mu.Unlock()
	if !connected {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent is not connected"})
	}

	j, err := st.newJob(in)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	st.mu.Lock()
	st.byToken[j.Token] = j
	st.byID[j.Claim.TaskID] = j
	err = st.assignLocked(j)
	view := j.view(true)
	st.mu.Unlock()
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent disconnected", "task": view})
	}
	log.Printf("assigned %s profile %s channel %s", j.Claim.TaskID, in.Profile, in.Channel)
	return c.Status(fiber.StatusAccepted).JSON(view)
}

func (st *state) newJob(in uploadIn) (*job, error) {
	sum, err := fileSHA(in.Video)
	if err != nil {
		return nil, err
	}
	ext := filepath.Ext(in.Video)
	now := time.Now()
	j := &job{
		Token: randHex(18), Sum: sum, Ext: ext,
		VideoPath: in.Video, VideoName: "video" + ext,
		Claim: &taskmcp.ClaimOut{
			Control: taskmcp.Continue, Kind: taskmcp.KindUpload,
			TaskID: "srv-" + randHex(4), Attempt: 1,
			FilePath: "video" + ext, ChannelID: in.Channel,
			ProfileDirectory: in.Profile,
			Metadata: &wire.Metadata{
				Title: in.Title, Description: in.Description, Visibility: wire.Visibility(in.Visibility),
			},
		},
		Status: wire.StatusQueued, CreatedAt: now, UpdatedAt: now,
	}
	if in.Thumbnail != "" {
		if _, err := os.Stat(in.Thumbnail); err != nil {
			return nil, err
		}
		j.ThumbPath = in.Thumbnail
		j.ThumbName = "thumbnail" + filepath.Ext(in.Thumbnail)
		j.Claim.ThumbnailPath = j.ThumbName
	}
	return j, nil
}

// assignLocked sends the job's current attempt to the agent and saves it.
// On a send error the job is marked FAILED. The caller holds st.mu.
func (st *state) assignLocked(j *job) error {
	err := st.sendLocked(wire.MsgAssign, st.assign(j))
	if err != nil {
		j.Status, j.Error, j.Stop = wire.StatusFailed, "assign: "+err.Error(), true
		j.addEvent(eventEntry{Event: "assign_failed", Message: err.Error()})
	} else {
		j.Status = wire.StatusAssigned
		j.addEvent(eventEntry{Event: "assigned"})
	}
	st.saveLocked()
	return err
}

func (st *state) file(c fiber.Ctx) error {
	st.mu.Lock()
	j := st.byID[c.Params("id")]
	st.mu.Unlock()
	if j == nil {
		return fiber.ErrNotFound
	}
	switch c.Params("name") {
	case j.VideoName:
		return c.SendFile(j.VideoPath)
	case j.ThumbName:
		if j.ThumbPath == "" {
			return fiber.ErrNotFound
		}
		return c.SendFile(j.ThumbPath)
	default:
		return fiber.ErrNotFound
	}
}

func (st *state) ws(conn *websocket.Conn) {
	st.mu.Lock()
	st.conn = conn
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		if st.conn == conn {
			st.conn = nil
		}
		st.mu.Unlock()
		conn.Close()
	}()
	for {
		var env wire.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			log.Printf("socket: %v", err)
			return
		}
		st.mu.Lock()
		if st.agent != nil {
			st.agent.LastSeen = time.Now()
		}
		st.mu.Unlock()
		switch env.Type {
		case wire.MsgHello:
			var hello wire.Hello
			if err := json.Unmarshal(env.Data, &hello); err != nil {
				log.Printf("hello: %v", err)
				continue
			}
			var dirs []string
			for _, p := range hello.Profiles {
				if p.Directory != "" {
					dirs = append(dirs, p.Directory)
				}
			}
			now := time.Now()
			st.mu.Lock()
			st.agent = &agentInfo{AgentID: hello.AgentID, Version: hello.Version, Profiles: hello.Profiles, ConnectedAt: now, LastSeen: now}
			st.mu.Unlock()
			log.Printf("agent %s profiles: %s", hello.AgentID, strings.Join(dirs, ", "))
		case wire.MsgReject:
			var rej wire.Reject
			if err := json.Unmarshal(env.Data, &rej); err != nil {
				log.Printf("reject: %v", err)
				continue
			}
			log.Printf("agent rejected %s: %s", rej.TaskID, rej.Reason)
			st.onReject(rej)
		case wire.MsgEvent:
			var ev wire.Event
			if err := json.Unmarshal(env.Data, &ev); err != nil {
				log.Printf("event: %v", err)
				continue
			}
			if ev.Type == wire.EventSessionEnded {
				st.onSessionEnded(ev)
			} else {
				log.Printf("event %s %s", ev.TaskID, ev.Type)
			}
		case wire.MsgHeartbeat:
			log.Printf("heartbeat %s", env.Data)
		default:
			log.Printf("message %s", env.Type)
		}
	}
}

// attemptLocked returns the job if ref is its current attempt.
func (st *state) attemptLocked(ref wire.TaskRef) *job {
	j := st.byID[ref.TaskID]
	if j == nil || (ref.Attempt != 0 && ref.Attempt != j.Claim.Attempt) {
		return nil
	}
	return j
}

func (st *state) onReject(rej wire.Reject) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.attemptLocked(rej.TaskRef)
	if j == nil {
		return
	}
	j.addEvent(eventEntry{Event: "rejected", Message: rej.Reason})
	if !j.Status.Terminal() {
		j.Status, j.Error, j.Stop = wire.StatusFailed, rej.Reason, true
	}
	st.saveLocked()
}

func (st *state) onSessionEnded(ev wire.Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.attemptLocked(ev.TaskRef)
	if j == nil {
		return
	}
	s := &sessionInfo{EndedAt: ev.At}
	if s.EndedAt.IsZero() {
		s.EndedAt = time.Now()
	}
	if v, ok := ev.Data["exit_code"].(float64); ok {
		s.ExitCode = int(v)
	}
	if v, ok := ev.Data["duration_ms"].(float64); ok {
		s.DurationMS = int64(v)
	}
	s.KilledBy, _ = ev.Data["killed_by"].(string)
	s.Error, _ = ev.Data["error"].(string)
	j.Session = s
	msg := "exit " + itoa(s.ExitCode)
	if s.KilledBy != "" {
		msg += ", killed by " + s.KilledBy
	}
	j.addEvent(eventEntry{Event: "session_ended", Message: msg})
	// The harness is gone. Without task_finish nobody will finish this attempt.
	if j.Finish == nil && !j.Status.Terminal() {
		j.Status, j.ErrorCode = wire.StatusLost, wire.ErrAgentLost
		j.Error = "session ended without task_finish"
		if s.Error != "" {
			j.Error += ": " + s.Error
		}
	}
	j.Stop = true
	log.Printf("session ended %s attempt %d: %s, finish_reported=%v", j.Claim.TaskID, j.Claim.Attempt, msg, j.Finish != nil)
	st.saveLocked()
}

func (st *state) assign(j *job) wire.Assign {
	task := wire.TaskSpec{
		TaskID: j.Claim.TaskID, Attempt: j.Claim.Attempt,
		ProfileDirectory: j.Claim.ProfileDirectory,
		FileURL:          st.base + "/files/" + j.Claim.TaskID + "/" + j.VideoName,
		SHA256:           j.Sum, FileExt: j.Ext,
		ExistingVideoID: j.Claim.ExistingVideoID,
		Deadline:        time.Now().Add(45 * time.Minute),
	}
	if j.ThumbName != "" {
		task.ThumbnailURL = st.base + "/files/" + j.Claim.TaskID + "/" + j.ThumbName
	}
	return wire.Assign{
		Task:      task,
		TaskMCP:   wire.MCPEndpoint{URL: st.base + "/task/mcp", Token: j.Token},
		ReportMCP: wire.MCPEndpoint{URL: st.base + "/report/mcp", Token: j.Token},
	}
}

func (st *state) Authenticate(_ context.Context, token string) (*taskmcp.Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.byToken[token]
	if j == nil {
		return nil, taskmcp.ErrUnauthorized
	}
	return &taskmcp.Session{ID: j.Claim.TaskID}, nil
}

func (st *state) jobOf(s *taskmcp.Session) (*job, error) {
	if s == nil {
		return nil, errors.New("missing session")
	}
	j := st.byID[s.ID]
	if j == nil {
		return nil, taskmcp.ErrUnauthorized
	}
	return j, nil
}

func (st *state) Claim(_ context.Context, s *taskmcp.Session) (*taskmcp.ClaimOut, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.jobOf(s)
	if err != nil {
		return nil, err
	}
	j.addEvent(eventEntry{Event: "task_claim"})
	st.saveLocked()
	if j.Stop {
		return &taskmcp.ClaimOut{Control: taskmcp.Stop, Message: "stopped"}, nil
	}
	log.Printf("task_claim -> %s attempt %d", j.Claim.TaskID, j.Claim.Attempt)
	out := *j.Claim
	return &out, nil
}

func (st *state) Report(_ context.Context, s *taskmcp.Session, in taskmcp.ReportIn) (*taskmcp.Ack, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.jobOf(s)
	if err != nil {
		return nil, err
	}
	log.Printf("task_report %s %s %d%% %s", j.Claim.TaskID, in.Step, in.Progress, in.Message)
	step := strings.ToUpper(strings.TrimSpace(in.Step))
	j.addEvent(eventEntry{Event: "task_report", Step: step, Progress: in.Progress, Message: in.Message})
	if !j.Stop {
		j.Step, j.Message = step, in.Message
		if in.Progress > 0 {
			j.Progress = in.Progress
		}
		if wire.Status(step).In(wire.RunningStatuses) {
			j.Status = wire.Status(step)
		}
	}
	st.saveLocked()
	return st.ackLocked(j), nil
}

func (st *state) VideoCreated(_ context.Context, s *taskmcp.Session, in taskmcp.VideoCreatedIn) (*taskmcp.Ack, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.jobOf(s)
	if err != nil {
		return nil, err
	}
	log.Printf("task_video_created %s %s %s", j.Claim.TaskID, in.VideoID, in.VideoURL)
	j.addEvent(eventEntry{Event: "task_video_created", VideoID: in.VideoID, Message: in.VideoURL})
	if in.VideoID != "" {
		j.VideoID, j.VideoURL = in.VideoID, in.VideoURL
		if j.VideoURL == "" {
			j.VideoURL = "https://youtu.be/" + in.VideoID
		}
	}
	st.saveLocked()
	return st.ackLocked(j), nil
}

func (st *state) Finish(_ context.Context, s *taskmcp.Session, in taskmcp.FinishIn) (*taskmcp.Ack, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.jobOf(s)
	if err != nil {
		return nil, err
	}
	log.Printf("task_finish %s %s video=%s code=%s reason=%s", j.Claim.TaskID, in.Status, in.VideoID, in.ErrorCode, in.Reason)
	j.addEvent(eventEntry{Event: "task_finish", Status: in.Status, VideoID: in.VideoID, Message: in.Reason})
	j.Finish = &finishInfo{Status: in.Status, ErrorCode: in.ErrorCode, Reason: in.Reason, VideoID: in.VideoID, At: time.Now()}
	if in.VideoID != "" {
		j.VideoID = in.VideoID
		if in.VideoURL != "" {
			j.VideoURL = in.VideoURL
		} else if j.VideoURL == "" {
			j.VideoURL = "https://youtu.be/" + in.VideoID
		}
	}
	// A cancelled task stays cancelled; the finish is still recorded.
	if j.Status != wire.StatusCancelled {
		switch strings.ToLower(in.Status) {
		case "done":
			j.Status, j.ErrorCode, j.Error = wire.StatusDone, "", ""
			j.Progress = 100
		case "needs_attention":
			j.Status, j.ErrorCode, j.Error = wire.StatusNeedsAttention, wire.ErrorCode(in.ErrorCode), in.Reason
		default:
			j.Status, j.ErrorCode, j.Error = wire.StatusFailed, wire.ErrorCode(in.ErrorCode), in.Reason
		}
	}
	j.Stop = true
	st.saveLocked()
	return &taskmcp.Ack{Control: taskmcp.Stop, Message: "recorded"}, nil
}

func (st *state) ackLocked(j *job) *taskmcp.Ack {
	if j.Stop {
		return &taskmcp.Ack{Control: taskmcp.Stop, Message: "stop"}
	}
	return &taskmcp.Ack{Control: taskmcp.Continue}
}

// profileNames lists the Chrome profile folders under profilesDir, which is
// a user-data-dir: only folders with a Preferences file are profiles, the
// rest (Safe Browsing, WidevineCdm, ...) are Chrome's own.
func (st *state) profileNames() ([]string, error) {
	entries, err := os.ReadDir(st.profilesDir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(st.profilesDir, name, "Preferences")); err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func validProfileName(name string) bool {
	return name != "" && name == filepath.Base(name) && name != "." && name != ".."
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
