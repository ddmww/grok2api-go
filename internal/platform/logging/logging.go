package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ddmww/grok2api-go/internal/platform/paths"
)

var (
	mu     sync.RWMutex
	logger *slog.Logger
)

func levelFromString(raw string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func Setup(level string, fileEnabled bool) *slog.Logger {
	mu.Lock()
	defer mu.Unlock()
	logger = newLogger(level, fileEnabled)
	return logger
}

func L() *slog.Logger {
	mu.RLock()
	current := logger
	mu.RUnlock()
	if current != nil {
		return current
	}
	return Setup("INFO", false)
}

func ReloadFileLogging(level string, enabled bool) *slog.Logger {
	return Setup(level, enabled)
}

func newLogger(level string, fileEnabled bool) *slog.Logger {
	writers := []io.Writer{os.Stdout}
	if fileEnabled {
		if err := os.MkdirAll(paths.LogDir(), 0o755); err == nil {
			filePath := filepath.Join(paths.LogDir(), "server.log")
			file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				writers = append(writers, file)
			}
		}
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: levelFromString(level),
	})
	return slog.New(handler)
}
