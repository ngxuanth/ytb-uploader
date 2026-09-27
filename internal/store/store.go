package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type State string

const (
	StatePending    State = "pending"
	StateQueued     State = "queued" // đã nằm trong asynq, kể cả lúc chờ retry
	StateProcessing State = "processing"
	StateDone       State = "done"
	StateError      State = "error"
	StateFailed     State = "failed" // hết số lần retry cho phép
)

// Job là toàn bộ thông tin về một tác vụ upload video, được lưu ở Redis Hash
// key "job:<id>" và index trong Sorted Set "jobs:index" (score = EnqueuedAt)
// để liệt kê theo thời gian.
type Job struct {
	ID            string    `json:"id"`
	FilePath      string    `json:"file_path"`
	State         State     `json:"state"`
	Attempt       int       `json:"attempt"`
	MaxAttempts   int       `json:"max_attempts"`
	EnqueuedAt    time.Time `json:"enqueued_at"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	VideoURL      string    `json:"video_url,omitempty"`
	Error         string    `json:"error,omitempty"`
	Profile       string    `json:"profile,omitempty"`
	ProfileDir    string    `json:"profile_dir,omitempty"`
	SlotAccounted bool      `json:"slot_accounted,omitempty"`
}

const (
	indexKey    = "jobs:index"
	cursorKey   = "scheduler:cursor"
	cooldownKey = "scheduler:cooldown_until"
	lockKey     = "scheduler:lock"
)

// unlockScript chỉ xóa lock khi token khớp, tránh process này xóa lock
// mà process khác vừa giành được sau khi TTL hết hạn.
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
`)

func jobKey(id string) string { return "job:" + id }

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

// Save ghi toàn bộ job (dùng khi tạo mới hoặc update) dưới dạng 1 field JSON
// duy nhất trong hash - đơn giản, tránh lệch field khi mở rộng struct sau này.
func (s *Store) Save(ctx context.Context, job *Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, jobKey(job.ID), "data", data)
	pipe.ZAdd(ctx, indexKey, redis.Z{
		Score:  float64(job.EnqueuedAt.Unix()),
		Member: job.ID,
	})
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("save job %s: %w", job.ID, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	data, err := s.rdb.HGet(ctx, jobKey(id), "data").Result()
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", id, err)
	}
	var job Job
	if err := json.Unmarshal([]byte(data), &job); err != nil {
		return nil, fmt.Errorf("unmarshal job %s: %w", id, err)
	}
	return &job, nil
}

// List trả về `limit` job gần nhất (mới nhất trước), dùng cho việc tra cứu
// "tiến trình đang thế nào" bất cứ lúc nào được hỏi.
func (s *Store) List(ctx context.Context, limit int64) ([]*Job, error) {
	ids, err := s.rdb.ZRevRange(ctx, indexKey, 0, limit-1).Result()
	if err != nil {
		return nil, fmt.Errorf("list job ids: %w", err)
	}
	jobs := make([]*Job, 0, len(ids))
	for _, id := range ids {
		j, err := s.Get(ctx, id)
		if err != nil {
			// job lẻ bị lỗi đọc không nên làm hỏng cả danh sách, log rồi bỏ qua
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// MarkFileSeen / IsFileSeen dùng để watcher không enqueue trùng 1 file nếu bị
// restart giữa chừng (dedupe theo đường dẫn tuyệt đối).
func (s *Store) MarkFileSeen(ctx context.Context, absPath string) error {
	return s.rdb.SAdd(ctx, "seen_files", absPath).Err()
}

func (s *Store) IsFileSeen(ctx context.Context, absPath string) (bool, error) {
	return s.rdb.SIsMember(ctx, "seen_files", absPath).Result()
}

func (s *Store) jobsOldest(ctx context.Context) ([]*Job, error) {
	ids, err := s.rdb.ZRange(ctx, indexKey, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("list job ids: %w", err)
	}
	jobs := make([]*Job, 0, len(ids))
	for _, id := range ids {
		j, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// OldestPending trả về job pending được ghi sớm nhất. nil nếu không còn.
func (s *Store) OldestPending(ctx context.Context) (*Job, error) {
	jobs, err := s.jobsOldest(ctx)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.State == StatePending {
			return j, nil
		}
	}
	return nil, nil
}

// HasPending báo còn video chưa được thả vào asynq hay không.
func (s *Store) HasPending(ctx context.Context) (bool, error) {
	job, err := s.OldestPending(ctx)
	if err != nil {
		return false, err
	}
	return job != nil, nil
}

// JobsWithState trả về mọi job ở đúng state, cũ nhất trước.
func (s *Store) JobsWithState(ctx context.Context, state State) ([]*Job, error) {
	jobs, err := s.jobsOldest(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Job, 0)
	for _, j := range jobs {
		if j.State == state {
			out = append(out, j)
		}
	}
	return out, nil
}

func (s *Store) GetCursor(ctx context.Context) (int, error) {
	v, err := s.rdb.Get(ctx, cursorKey).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get cursor: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("cursor khong hop le: %w", err)
	}
	return n, nil
}

// GetCooldownUntil trả về thời điểm được phép bắt đầu vòng profile tiếp theo.
// Zero time nghĩa là không có cooldown.
func (s *Store) GetCooldownUntil(ctx context.Context) (time.Time, error) {
	v, err := s.rdb.Get(ctx, cooldownKey).Result()
	if err == redis.Nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get cooldown: %w", err)
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("cooldown khong hop le: %w", err)
	}
	return time.Unix(sec, 0).UTC(), nil
}

func (s *Store) TryLock(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, lockKey, token, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("scheduler lock: %w", err)
	}
	return ok, nil
}

func (s *Store) Unlock(ctx context.Context, token string) error {
	err := unlockScript.Run(ctx, s.rdb, []string{lockKey}, token).Err()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scheduler unlock: %w", err)
	}
	return nil
}

// CommitSlot ghi trạng thái kết thúc của job cùng cursor (và cooldown nếu có)
// trong một transaction, để dispatcher không thấy "hết task" trước khi cursor tăng.
func (s *Store) CommitSlot(ctx context.Context, job *Job, newCursor int, cooldownUntil time.Time, setCooldown bool) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, jobKey(job.ID), "data", data)
	pipe.ZAdd(ctx, indexKey, redis.Z{
		Score:  float64(job.EnqueuedAt.Unix()),
		Member: job.ID,
	})
	pipe.Set(ctx, cursorKey, strconv.Itoa(newCursor), 0)
	if setCooldown {
		pipe.Set(ctx, cooldownKey, strconv.FormatInt(cooldownUntil.Unix(), 10), 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("commit slot %s: %w", job.ID, err)
	}
	return nil
}
