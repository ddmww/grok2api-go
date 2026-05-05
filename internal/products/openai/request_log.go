package openai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/model"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/logstream"
)

type requestLog struct {
	state    *app.State
	category logstream.Category
	path     string
	model    string
	sso      string
	start    time.Time
}

func newRequestLog(state *app.State, category logstream.Category, path string, spec model.Spec) requestLog {
	return requestLog{
		state:    state,
		category: category,
		path:     path,
		model:    spec.Name,
		start:    time.Now(),
	}
}

func (l *requestLog) setLease(lease *account.Lease) {
	if lease == nil {
		return
	}
	l.sso = logstream.MaskSSO(lease.Token)
}

func (l requestLog) success(message string) {
	l.add(logstream.Event{
		Category:   l.category,
		Level:      logstream.LevelInfo,
		Path:       l.path,
		Model:      l.model,
		StatusCode: http.StatusOK,
		DurationMS: time.Since(l.start).Milliseconds(),
		SSO:        l.sso,
		Message:    message,
	})
}

func (l requestLog) failure(err error, message string) {
	level := logstream.LevelWarn
	status := http.StatusInternalServerError
	category := l.category
	if errors.Is(err, context.Canceled) {
		status = 499
	}
	var upstream *xai.UpstreamError
	if errors.As(err, &upstream) {
		status = upstream.Status
	}
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError || status == http.StatusUnauthorized || status == http.StatusForbidden {
		level = logstream.LevelError
	}
	if level == logstream.LevelError {
		category = logstream.CategoryError
	}
	l.add(logstream.Event{
		Category:     category,
		Level:        level,
		Path:         l.path,
		Model:        l.model,
		StatusCode:   status,
		DurationMS:   time.Since(l.start).Milliseconds(),
		SSO:          l.sso,
		ErrorSummary: errorSummary(err),
		Message:      message,
	})
}

func (l requestLog) rateLimited(reason string) {
	l.add(logstream.Event{
		Category:     logstream.CategoryError,
		Level:        logstream.LevelError,
		Path:         l.path,
		Model:        l.model,
		StatusCode:   http.StatusTooManyRequests,
		DurationMS:   time.Since(l.start).Milliseconds(),
		SSO:          l.sso,
		ErrorSummary: reason,
		Message:      "sso persisted as rate limited",
	})
}

func (l requestLog) add(event logstream.Event) {
	if l.state == nil || l.state.Logs == nil {
		return
	}
	l.state.Logs.Add(event)
}

func errorSummary(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}
