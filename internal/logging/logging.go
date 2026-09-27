package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// New tạo logger dạng JSON, ghi đồng thời ra stdout (để xem trực tiếp / systemd
// journal bắt được) và ra file trong logDir/<component>.log (để tra cứu lại sau).
// Mỗi dòng log là 1 JSON object -> dễ grep, dễ đẩy vào ELK/Loki sau này nếu cần.
func New(logDir, component string) (*slog.Logger, func() error, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, nil, err
	}

	logPath := filepath.Join(logDir, component+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}

	writer := io.MultiWriter(os.Stdout, f)

	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: true,
	})

	logger := slog.New(handler).With(
		slog.String("component", component),
	)

	return logger, f.Close, nil
}
