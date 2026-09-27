// Package scheduler quyết định khi nào một video được thả vào asynq.
// Asynq chỉ thực thi task (retry, timeout). Vòng profile và cooldown 1 giờ
// nằm ở đây, vì queue weight / rate limit / ProcessAt lúc enqueue không
// diễn tả được "hết một vòng profile thì nghỉ".
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hibiken/asynq"

	"yt-uploader/internal/config"
	"yt-uploader/internal/queue"
	"yt-uploader/internal/store"
)

const (
	dispatchInterval = 5 * time.Second
	lockTTL          = 30 * time.Second
	lockWait         = 15 * time.Second
)

type Scheduler struct {
	cfg       config.Config
	logger    *slog.Logger
	st        *store.Store
	client    *asynq.Client
	inspector *asynq.Inspector
	profiles  []config.Profile
}

func New(cfg config.Config, logger *slog.Logger, st *store.Store, client *asynq.Client) *Scheduler {
	opt := asynq.RedisClientOpt{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	}
	return &Scheduler{
		cfg:       cfg,
		logger:    logger,
		st:        st,
		client:    client,
		inspector: asynq.NewInspector(opt),
		profiles:  cfg.Profiles,
	}
}

func (s *Scheduler) Close() error {
	return s.inspector.Close()
}

// Run gọi Dispatch ngay, rồi lặp mỗi vài giây cho đến khi ctx bị hủy.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("dispatcher bat dau",
		slog.Any("profiles", s.profiles),
		slog.Duration("round_cooldown", s.cfg.RoundCooldown),
	)
	s.Dispatch(ctx)

	ticker := time.NewTicker(dispatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("dispatcher dung")
			return
		case <-ticker.C:
			s.Dispatch(ctx)
		}
	}
}

// Dispatch thả đúng một job pending vào asynq nếu không có task đang chạy
// và đã hết cooldown. Cursor không tăng ở đây — chỉ tăng khi slot kết thúc.
func (s *Scheduler) Dispatch(ctx context.Context) {
	err := s.withLock(ctx, func() error {
		if len(s.profiles) == 0 {
			return fmt.Errorf("khong co profile")
		}

		processing, err := s.st.JobsWithState(ctx, store.StateProcessing)
		if err != nil {
			return err
		}
		if len(processing) > 0 {
			// Worker chết giữa chừng để job kẹt processing và không còn task asynq.
			// Đưa về pending để video sau không bị chặn mãi.
			live, err := s.releaseAbandoned(ctx, processing)
			if err != nil {
				return err
			}
			if live {
				return nil
			}
		}

		queued, err := s.st.JobsWithState(ctx, store.StateQueued)
		if err != nil {
			return err
		}
		if len(queued) > 0 {
			// Job đã gán profile. Nếu process chết trước Enqueue, task chưa có
			// trong asynq — enqueue lại cùng TaskID, không tăng cursor.
			return s.ensureEnqueued(ctx, queued)
		}

		until, err := s.st.GetCooldownUntil(ctx)
		if err != nil {
			return err
		}
		if !until.IsZero() && time.Now().Before(until) {
			return nil
		}

		job, err := s.st.OldestPending(ctx)
		if err != nil {
			return err
		}
		if job == nil {
			return nil
		}

		cursor, err := s.st.GetCursor(ctx)
		if err != nil {
			return err
		}
		profile := s.profiles[cursor%len(s.profiles)]
		job.Profile = profile.Name
		job.ProfileDir = profile.Dir
		job.State = store.StateQueued
		if err := s.st.Save(ctx, job); err != nil {
			return err
		}

		task, err := s.buildTask(job)
		if err != nil {
			s.revertPending(ctx, job)
			return err
		}

		info, err := s.client.EnqueueContext(ctx, task)
		if err != nil {
			if errors.Is(err, asynq.ErrTaskIDConflict) {
				s.logger.Info("task da co trong asynq, giu trang thai queued",
					slog.String("job_id", job.ID),
					slog.String("profile", profile.Name),
				)
				return nil
			}
			s.revertPending(ctx, job)
			return err
		}

		s.logger.Info("da enqueue job theo profile",
			slog.String("job_id", job.ID),
			slog.String("asynq_task_id", info.ID),
			slog.String("profile", profile.Name),
			slog.String("profile_dir", profile.Dir),
			slog.String("file", job.FilePath),
			slog.Int("cursor", cursor),
		)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.logger.Error("dispatcher loi", slog.String("error", err.Error()))
	}
}

// FinishSlot đóng một lượt profile: ghi done/failed, tăng cursor.
// Hết một vòng (cursor chia hết cho số profile) và vẫn còn job pending
// thì đặt cooldown. Retry giữa chừng không gọi hàm này.
// Gọi lại sau khi đã chốt thì không tăng cursor lần nữa.
func (s *Scheduler) FinishSlot(ctx context.Context, job *store.Job, state store.State) error {
	return s.withLock(ctx, func() error {
		current, err := s.st.Get(ctx, job.ID)
		if err != nil {
			current = job
		}
		if current.SlotAccounted {
			return nil
		}

		current.State = state
		current.SlotAccounted = true
		current.Profile = job.Profile
		current.ProfileDir = job.ProfileDir
		current.VideoURL = job.VideoURL
		current.Error = job.Error
		current.FinishedAt = job.FinishedAt
		current.Attempt = job.Attempt
		if current.FinishedAt.IsZero() {
			current.FinishedAt = time.Now().UTC()
		}

		cursor, err := s.st.GetCursor(ctx)
		if err != nil {
			return err
		}
		newCursor := cursor + 1

		setCooldown := false
		var until time.Time
		n := len(s.profiles)
		if n > 0 && newCursor%n == 0 {
			pending, err := s.st.HasPending(ctx)
			if err != nil {
				return err
			}
			if pending {
				setCooldown = true
				until = time.Now().UTC().Add(s.cfg.RoundCooldown)
			}
		}

		if err := s.st.CommitSlot(ctx, current, newCursor, until, setCooldown); err != nil {
			return err
		}

		s.logger.Info("da chot slot",
			slog.String("job_id", current.ID),
			slog.String("profile", current.Profile),
			slog.String("state", string(state)),
			slog.Int("cursor", newCursor),
			slog.Bool("cooldown", setCooldown),
		)
		if setCooldown {
			s.logger.Info("het mot vong profile, nghi truoc khi upload tiep",
				slog.Time("cooldown_until", until),
				slog.Duration("cooldown", s.cfg.RoundCooldown),
			)
		}
		return nil
	})
}

// releaseAbandoned đưa job processing không còn task asynq về pending.
// Trả về true nếu vẫn còn ít nhất một task đang sống.
func (s *Scheduler) releaseAbandoned(ctx context.Context, jobs []*store.Job) (bool, error) {
	live := false
	for _, job := range jobs {
		_, err := s.inspector.GetTaskInfo(queue.QueueName, job.ID)
		if err == nil {
			live = true
			continue
		}
		if !errors.Is(err, asynq.ErrTaskNotFound) && !errors.Is(err, asynq.ErrQueueNotFound) {
			return false, err
		}
		job.State = store.StatePending
		job.Profile = ""
		job.ProfileDir = ""
		if err := s.st.Save(ctx, job); err != nil {
			return false, err
		}
		s.logger.Warn("job processing khong con task, dua ve pending",
			slog.String("job_id", job.ID),
			slog.String("file", job.FilePath),
		)
	}
	return live, nil
}

func (s *Scheduler) ensureEnqueued(ctx context.Context, jobs []*store.Job) error {
	for _, job := range jobs {
		_, err := s.inspector.GetTaskInfo(queue.QueueName, job.ID)
		if err == nil {
			continue
		}
		// Queue chưa từng có task cũng coi như task bị mất: enqueue sẽ tạo queue.
		if !errors.Is(err, asynq.ErrTaskNotFound) && !errors.Is(err, asynq.ErrQueueNotFound) {
			return err
		}

		task, err := s.buildTask(job)
		if err != nil {
			return err
		}
		info, err := s.client.EnqueueContext(ctx, task)
		if err != nil {
			if errors.Is(err, asynq.ErrTaskIDConflict) {
				continue
			}
			return err
		}
		s.logger.Info("enqueue lai task bi mat",
			slog.String("job_id", job.ID),
			slog.String("asynq_task_id", info.ID),
			slog.String("profile", job.Profile),
		)
	}
	return nil
}

func (s *Scheduler) buildTask(job *store.Job) (*asynq.Task, error) {
	maxRetry := s.cfg.MaxAttempts - 1
	if maxRetry < 0 {
		maxRetry = 0
	}
	return queue.BuildTask(queue.VideoUploadPayload{
		JobID:      job.ID,
		FilePath:   job.FilePath,
		Profile:    job.Profile,
		ProfileDir: job.ProfileDir,
	}, maxRetry, s.cfg.AgentTimeout)
}

func (s *Scheduler) revertPending(ctx context.Context, job *store.Job) {
	job.State = store.StatePending
	job.Profile = ""
	job.ProfileDir = ""
	if err := s.st.Save(ctx, job); err != nil {
		s.logger.Error("khong revert duoc job ve pending",
			slog.String("job_id", job.ID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Scheduler) withLock(ctx context.Context, fn func() error) error {
	token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	deadline := time.Now().Add(lockWait)
	for {
		ok, err := s.st.TryLock(ctx, token, lockTTL)
		if err != nil {
			return err
		}
		if ok {
			defer func() {
				if err := s.st.Unlock(context.Background(), token); err != nil {
					s.logger.Warn("khong tha duoc scheduler lock", slog.String("error", err.Error()))
				}
			}()
			return fn()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("khong lay duoc scheduler lock")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
