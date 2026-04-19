package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/ddmww/grok2api-go/internal/platform/paths"
)

var (
	once   sync.Once
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
	once.Do(func() {
		writers := []io.Writer{os.Stdout}
		if fileEnabled {
			if err := os.MkdirAll(paths.LogDir(), 0o755); err == nil {
				file, err := os.OpenFile(paths.LogDir()+string(os.PathSeparator)+"server.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if err == nil {
					writers = append(writers, file)
				}
			}
		}
		handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
			Level: levelFromString(level),
		})
		logger = slog.New(handler)
	})
	return logger
}

func L() *slog.Logger {
	if logger == nil {
		return Setup("INFO", false)
	}
	return logger
}
