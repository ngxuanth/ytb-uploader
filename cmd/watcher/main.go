// Worker #1: watch folder video, khi phát hiện file mới và đã ghi xong,
// tạo job trạng thái "pending" trong Redis. Dispatcher trong worker mới
// enqueue asynq, theo vòng profile và cooldown.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"yt-uploader/internal/config"
	"yt-uploader/internal/logging"
	"yt-uploader/internal/store"
)

var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".avi": true, ".webm": true,
}

type fileState struct {
	lastSize    int64
	stableCount int
}

func main() {
	cfg := config.Load()

	logger, closeLog, err := logging.New(cfg.LogDir, "watcher")
	if err != nil {
		fmt.Fprintf(os.Stderr, "khong the khoi tao logger: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	logger.Info("watcher starting",
		slog.String("watch_dir", cfg.WatchDir),
		slog.Duration("poll_interval", cfg.PollInterval),
		slog.Int("stable_checks_needed", cfg.StableChecksNeeded),
	)

	if err := os.MkdirAll(cfg.WatchDir, 0o755); err != nil {
		logger.Error("khong tao duoc watch dir", slog.String("error", err.Error()))
		os.Exit(1)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	defer rdb.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Error("khong ket noi duoc redis", slog.String("redis_addr", cfg.RedisAddr), slog.String("error", err.Error()))
		os.Exit(1)
	}
	logger.Info("da ket noi redis thanh cong", slog.String("redis_addr", cfg.RedisAddr))

	st := store.New(rdb)

	tracking := make(map[string]*fileState)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	logger.Info("watcher da san sang, bat dau vong lap quet folder")

	for {
		select {
		case <-ctx.Done():
			logger.Info("nhan tin hieu dung, watcher thoat")
			return
		case <-ticker.C:
			scanOnce(ctx, cfg, logger, st, tracking)
		}
	}
}

func scanOnce(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	st *store.Store,
	tracking map[string]*fileState,
) {
	entries, err := os.ReadDir(cfg.WatchDir)
	if err != nil {
		logger.Error("khong doc duoc watch dir", slog.String("error", err.Error()))
		return
	}

	seenThisScan := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if !videoExts[ext] {
			continue
		}

		absPath, err := filepath.Abs(filepath.Join(cfg.WatchDir, entry.Name()))
		if err != nil {
			logger.Warn("khong resolve duoc duong dan tuyet doi", slog.String("file", entry.Name()), slog.String("error", err.Error()))
			continue
		}
		seenThisScan[absPath] = true

		info, err := entry.Info()
		if err != nil {
			logger.Warn("khong lay duoc thong tin file", slog.String("file", absPath), slog.String("error", err.Error()))
			continue
		}

		alreadySeen, err := st.IsFileSeen(ctx, absPath)
		if err != nil {
			logger.Error("loi kiem tra seen_files tren redis", slog.String("file", absPath), slog.String("error", err.Error()))
			continue
		}
		if alreadySeen {
			continue
		}

		fs, tracked := tracking[absPath]
		if !tracked {
			fs = &fileState{lastSize: info.Size(), stableCount: 1}
			tracking[absPath] = fs
			logger.Info("phat hien file moi, bat dau theo doi kich thuoc",
				slog.String("file", absPath), slog.Int64("size_bytes", info.Size()))
			continue
		}

		if info.Size() == fs.lastSize {
			fs.stableCount++
		} else {
			fs.lastSize = info.Size()
			fs.stableCount = 1
		}

		if fs.stableCount >= cfg.StableChecksNeeded {
			recordPending(ctx, cfg, logger, st, absPath, info.Size())
			delete(tracking, absPath)
		}
	}

	for path := range tracking {
		if !seenThisScan[path] {
			logger.Warn("file dang theo doi bien mat khoi folder truoc khi ghi xong", slog.String("file", path))
			delete(tracking, path)
		}
	}
}

// recordPending chỉ ghi job pending. Dispatcher trong worker mới enqueue asynq,
// để round-robin profile và cooldown không bị queue kéo đi hết ngay.
func recordPending(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	st *store.Store,
	absPath string,
	size int64,
) {
	jobID := newJobID()
	now := time.Now().UTC()

	job := &store.Job{
		ID:          jobID,
		FilePath:    absPath,
		State:       store.StatePending,
		Attempt:     0,
		MaxAttempts: cfg.MaxAttempts,
		EnqueuedAt:  now,
	}
	if err := st.Save(ctx, job); err != nil {
		logger.Error("khong luu duoc job vao redis, se thu lai o lan quet sau",
			slog.String("file", absPath), slog.String("error", err.Error()))
		return
	}

	if err := st.MarkFileSeen(ctx, absPath); err != nil {
		logger.Warn("khong danh dau duoc file la da seen", slog.String("file", absPath), slog.String("error", err.Error()))
	}

	logger.Info("da ghi job pending",
		slog.String("job_id", jobID),
		slog.String("file", absPath),
		slog.Int64("size_bytes", size),
	)
}

func newJobID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}
