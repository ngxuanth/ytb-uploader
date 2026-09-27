// Worker #2: nhận task qua asynq.Server, gọi harness cấu hình từ bên ngoài
// (HARNESS_BIN / HARNESS_ARGS) với prompt chuẩn để upload video, rồi cập nhật
// trạng thái job trong Redis.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"yt-uploader/internal/config"
	"yt-uploader/internal/harness"
	"yt-uploader/internal/logging"
	"yt-uploader/internal/queue"
	"yt-uploader/internal/scheduler"
	"yt-uploader/internal/store"
)

type taskHandler struct {
	cfg    config.Config
	logger *slog.Logger
	st     *store.Store
	sched  *scheduler.Scheduler
}

func main() {
	cfg := config.Load()

	logger, closeLog, err := logging.New(cfg.LogDir, "worker")
	if err != nil {
		fmt.Fprintf(os.Stderr, "khong the khoi tao logger: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	defer rdb.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		logger.Error("khong ket noi duoc redis", slog.String("redis_addr", cfg.RedisAddr), slog.String("error", err.Error()))
		os.Exit(1)
	}
	logger.Info("da ket noi redis thanh cong", slog.String("redis_addr", cfg.RedisAddr))

	if len(cfg.Profiles) == 0 {
		logger.Error("khong tim thay chrome profile", slog.String("chrome_profiles_dir", cfg.ChromeProfilesDir))
		os.Exit(1)
	}
	if cfg.HarnessBin == "" {
		logger.Error("HARNESS_BIN trong, dat lenh agent muon dung")
		os.Exit(1)
	}

	st := store.New(rdb)

	asynqClient := queue.NewClient(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	defer asynqClient.Close()

	sched := scheduler.New(cfg, logger, st, asynqClient)
	defer sched.Close()
	// Process con (harness, Chrome) nằm trong job này và chết theo worker.
	if err := ensureKillTree(); err != nil {
		logger.Warn("khong gan job object, process con co the sot lai", slog.String("error", err.Error()))
	}
	dispCtx, stopDisp := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopDisp()
	go sched.Run(dispCtx)

	srv := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		},
		asynq.Config{
			// Một BrowserMCP: upload lần lượt. Round-robin profile do dispatcher
			// gán, không tăng concurrency theo số profile.
			Concurrency: 1,
			Queues: map[string]int{
				queue.QueueName: 1,
			},
			Logger: logging.AsynqLogger{L: logger},
		},
	)

	mux := asynq.NewServeMux()
	h := &taskHandler{cfg: cfg, logger: logger, st: st, sched: sched}
	mux.HandleFunc(queue.TypeVideoUpload, h.HandleVideoUpload)

	logger.Info("worker starting",
		slog.String("harness_bin", cfg.HarnessBin),
		slog.Any("harness_args", cfg.HarnessArgs),
		slog.Duration("agent_timeout", cfg.AgentTimeout),
		slog.Int("max_attempts", cfg.MaxAttempts),
		slog.Any("profiles", cfg.Profiles),
		slog.Duration("round_cooldown", cfg.RoundCooldown),
	)

	// Không dùng srv.Run: trên Windows nó chờ windows.SIGINT, kiểu signal Go
	// không phát khi Ctrl+C, nên process không thoát và giữ file log.
	if err := srv.Start(mux); err != nil {
		logger.Error("asynq server dung voi loi", slog.String("error", err.Error()))
		os.Exit(1)
	}

	<-dispCtx.Done()
	logger.Info("nhan tin hieu dung")
	forceCtx, stopForce := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopForce()
	done := make(chan struct{})
	go func() {
		srv.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-forceCtx.Done():
		logger.Error("nhan tin hieu lan nua, thoat ngay")
		os.Exit(1)
	case <-time.After(8 * time.Second):
		logger.Error("shutdown qua han, thoat")
		os.Exit(1)
	}
}

func (h *taskHandler) HandleVideoUpload(ctx context.Context, t *asynq.Task) error {
	var payload queue.VideoUploadPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("khong parse duoc payload: %w", err)
	}

	retryCount, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	taskID, _ := asynq.GetTaskID(ctx)
	isLastAttempt := retryCount >= maxRetry

	log := h.logger.With(
		slog.String("job_id", payload.JobID),
		slog.String("asynq_task_id", taskID),
		slog.String("profile", payload.Profile),
		slog.Int("retry_count", retryCount),
		slog.Int("max_retry", maxRetry),
	)

	job, err := h.st.Get(ctx, payload.JobID)
	if err != nil {
		log.Warn("khong doc duoc job tu store, tao lai tu payload", slog.String("error", err.Error()))
		job = &store.Job{
			ID:          payload.JobID,
			FilePath:    payload.FilePath,
			Profile:     payload.Profile,
			ProfileDir:  payload.ProfileDir,
			MaxAttempts: maxRetry + 1,
			EnqueuedAt:  time.Now().UTC(),
		}
	}

	// Slot đã chốt rồi (crash sau CommitSlot): không upload lại, không tăng cursor.
	if job.SlotAccounted {
		if job.State == store.StateFailed {
			return fmt.Errorf("upload that bai: %s", job.Error)
		}
		log.Info("slot da chot, bo qua")
		return nil
	}

	profile := config.Profile{Name: payload.Profile, Dir: payload.ProfileDir}
	if profile.Name == "" {
		profile.Name = job.Profile
		profile.Dir = job.ProfileDir
	}
	job.Profile = profile.Name
	job.ProfileDir = profile.Dir

	// URL đã lưu nhưng slot chưa chốt (Redis lỗi sau khi agent xong): không upload lại.
	if job.VideoURL != "" {
		log.Info("da co video url, chi chot slot")
		if job.FinishedAt.IsZero() {
			job.FinishedAt = time.Now().UTC()
		}
		if err := h.finishSlot(ctx, log, job, store.StateDone); err != nil {
			return fmt.Errorf("khong chot slot: %w", err)
		}
		return nil
	}

	job.Attempt = retryCount + 1
	job.State = store.StateProcessing
	job.StartedAt = time.Now().UTC()
	if err := h.st.Save(ctx, job); err != nil {
		log.Error("khong luu duoc trang thai processing", slog.String("error", err.Error()))
	}

	log.Info("bat dau xu ly job",
		slog.String("jobID", job.ID),
		slog.String("file", job.FilePath),
		slog.String("profile", profile.Name),
		slog.String("profile_dir", profile.Dir),
	)

	result, runErr := harness.Run(ctx, h.cfg, log, harness.Input{
		File:    job.FilePath,
		Profile: profile,
	})
	job.FinishedAt = time.Now().UTC()
	duration := job.FinishedAt.Sub(job.StartedAt)

	if runErr == nil && result != nil && result.Status == "done" {
		job.VideoURL = result.URL
		job.Error = ""
		if err := h.st.Save(ctx, job); err != nil {
			log.Error("khong luu duoc video url truoc khi chot slot", slog.String("error", err.Error()))
		}
		if err := h.finishSlot(ctx, log, job, store.StateDone); err != nil {
			log.Error("upload xong nhung khong chot duoc slot", slog.String("error", err.Error()))
			return fmt.Errorf("upload xong nhung khong chot slot: %w", err)
		}
		log.Info("upload thanh cong",
			slog.String("video_url", result.URL),
			slog.String("profile", profile.Name),
			slog.Duration("duration", duration),
		)
		return nil
	}

	errMsg := ""
	switch {
	case runErr != nil:
		errMsg = runErr.Error()
	case result != nil:
		errMsg = result.Reason
	default:
		errMsg = "khong ro nguyen nhan"
	}
	job.Error = errMsg

	if isLastAttempt {
		if err := h.finishSlot(ctx, log, job, store.StateFailed); err != nil {
			log.Error("job that bai va khong chot duoc slot", slog.String("error", err.Error()))
			return fmt.Errorf("upload that bai va khong chot slot: %w", err)
		}
		log.Error("job that bai vinh vien, da het so lan retry cua asynq",
			slog.String("error", errMsg), slog.Duration("duration", duration))
		return fmt.Errorf("upload that bai: %s", errMsg)
	}

	// Giữ queued để dispatcher không thả video khác trong lúc asynq chờ retry.
	// Cùng profile trong payload, không tăng cursor.
	job.State = store.StateQueued
	if err := h.st.Save(ctx, job); err != nil {
		log.Error("khong luu duoc trang thai loi", slog.String("error", err.Error()))
	}
	log.Warn("job that bai, asynq se tu dong retry sau (co backoff)",
		slog.String("error", errMsg), slog.Duration("duration", duration))
	return fmt.Errorf("upload that bai: %s", errMsg)
}

func (h *taskHandler) finishSlot(ctx context.Context, log *slog.Logger, job *store.Job, state store.State) error {
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		err = h.sched.FinishSlot(ctx, job, state)
		if err == nil {
			return nil
		}
		log.Warn("chua chot duoc slot, thu lai",
			slog.Int("try", attempt),
			slog.String("error", err.Error()),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
	}
	return err
}
