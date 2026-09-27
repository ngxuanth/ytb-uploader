package logging

import (
	"fmt"
	"log/slog"
	"os"
)

// AsynqLogger bọc slog.Logger để asynq.Server dùng làm logger nội bộ, giúp
// log của asynq (task started/processed/retry, server start/stop...) đi
// chung format JSON với toàn bộ log còn lại của hệ thống.
type AsynqLogger struct {
	L *slog.Logger
}

func (a AsynqLogger) Debug(args ...interface{}) { a.L.Debug(fmt.Sprint(args...)) }
func (a AsynqLogger) Info(args ...interface{})  { a.L.Info(fmt.Sprint(args...)) }
func (a AsynqLogger) Warn(args ...interface{})  { a.L.Warn(fmt.Sprint(args...)) }
func (a AsynqLogger) Error(args ...interface{}) { a.L.Error(fmt.Sprint(args...)) }
func (a AsynqLogger) Fatal(args ...interface{}) {
	a.L.Error(fmt.Sprint(args...))
	os.Exit(1)
}
