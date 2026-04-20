package upstreamblocker

import (
	"fmt"
	"strings"

	"github.com/ddmww/grok2api-go/internal/platform/config"
)

const DefaultMessage = "上游渠道商拦截了当前请求，请尝试换个说法后重试，或稍后再试。"

type Config struct {
	Enabled       bool
	CaseSensitive bool
	Keywords      []string
	Message       string
}

type Error struct {
	MatchedKeyword string
	Message        string
}

func (e *Error) Error() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return DefaultMessage
	}
	return e.Message
}

func NormalizeKeywords(keywords any) []string {
	var items []any
	switch typed := keywords.(type) {
	case string:
		lines := strings.Split(typed, "\n")
		items = make([]any, 0, len(lines))
		for _, line := range lines {
			items = append(items, line)
		}
	case []string:
		items = make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
	case []any:
		items = typed
	default:
		return []string{}
	}

	normalized := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		keyword := strings.TrimSpace(fmt.Sprint(item))
		if keyword == "" {
			continue
		}
		if _, ok := seen[keyword]; ok {
			continue
		}
		seen[keyword] = struct{}{}
		normalized = append(normalized, keyword)
	}
	return normalized
}

func GetConfig(cfg *config.Service) Config {
	if cfg == nil {
		return Config{Message: DefaultMessage}
	}
	message := strings.TrimSpace(cfg.GetString("upstream_blocker.message", DefaultMessage))
	if message == "" {
		message = DefaultMessage
	}
	return Config{
		Enabled:       cfg.GetBool("upstream_blocker.enabled", false),
		CaseSensitive: cfg.GetBool("upstream_blocker.case_sensitive", false),
		Keywords:      NormalizeKeywords(cfg.Get("upstream_blocker.keywords")),
		Message:       message,
	}
}

func FindBlockedKeyword(cfg Config, text string) string {
	candidate := strings.TrimSpace(text)
	if candidate == "" || !cfg.Enabled {
		return ""
	}
	haystack := candidate
	if !cfg.CaseSensitive {
		haystack = strings.ToLower(candidate)
	}
	for _, keyword := range cfg.Keywords {
		needle := keyword
		if !cfg.CaseSensitive {
			needle = strings.ToLower(keyword)
		}
		if needle != "" && strings.Contains(haystack, needle) {
			return keyword
		}
	}
	return ""
}

func AssertResponseAllowed(cfg Config, text string, source string) error {
	matched := FindBlockedKeyword(cfg, text)
	if matched == "" {
		return nil
	}
	return &Error{
		MatchedKeyword: matched,
		Message:        cfg.Message,
	}
}
