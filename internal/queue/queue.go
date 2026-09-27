package queue

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

// TypeVideoUpload là tên loại task, dùng để đăng ký handler tương ứng bên worker.
const TypeVideoUpload = "video:upload"

// QueueName gom toàn bộ task upload video vào 1 queue riêng, để dễ set
// concurrency/priority độc lập nếu sau này thêm loại task khác.
const QueueName = "video_upload"

// VideoUploadPayload là dữ liệu đính kèm theo mỗi task; worker đọc lại để
// biết cần upload file nào, job nào, và đúng một profile đã được scheduler gán.
// Profile không đổi khi asynq retry: một video không sang kênh khác.
type VideoUploadPayload struct {
	JobID      string `json:"job_id"`
	FilePath   string `json:"file_path"`
	Profile    string `json:"profile"`
	ProfileDir string `json:"profile_dir,omitempty"`
}

// NewClient tạo asynq.Client dùng để enqueue task (dùng trong watcher).
func NewClient(addr, password string, db int) *asynq.Client {
	return asynq.NewClient(asynq.RedisClientOpt{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
}

// BuildTask tạo 1 task upload video kèm option retry/timeout.
//
// LƯU Ý: asynq.MaxRetry(n) là SỐ LẦN RETRY sau lần chạy đầu tiên, tức tổng
// số lần chạy tối đa = n + 1. Nếu muốn tổng cộng tối đa N lần chạy (giống
// ý nghĩa MaxAttempts trong config), truyền maxRetry = N - 1.
func BuildTask(payload VideoUploadPayload, maxRetry int, timeout time.Duration) (*asynq.Task, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	task := asynq.NewTask(
		TypeVideoUpload,
		data,
		asynq.MaxRetry(maxRetry),
		asynq.Timeout(timeout),
		asynq.Queue(QueueName),
		// Cố định ID theo job để dispatcher không enqueue trùng sau restart.
		asynq.TaskID(payload.JobID),
	)
	return task, nil
}
