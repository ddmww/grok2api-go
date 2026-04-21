package xai

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	summaryTagRe   = regexp.MustCompile(`(?is)<[^>]+>`)
	summarySpaceRe = regexp.MustCompile(`\s+`)
)

type thinkingSummary struct {
	raw strings.Builder
}

func (s *thinkingSummary) Add(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	s.raw.WriteString(token)
	if !strings.HasSuffix(token, "\n") {
		s.raw.WriteString("\n")
	}
}

func (s *thinkingSummary) Flush() string {
	raw := strings.TrimSpace(s.raw.String())
	s.raw.Reset()
	if raw == "" {
		return ""
	}
	return summarizeThinking(raw)
}

func summarizeThinking(raw string) string {
	raw = summaryTagRe.ReplaceAllString(raw, " ")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, "\t", " ")
	raw = summarySpaceRe.ReplaceAllString(raw, " ")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	parts := splitThinkingClauses(raw)
	if len(parts) == 0 {
		return truncateThinking(raw, 220)
	}

	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, part := range parts {
		part = normalizeThinkingClause(part)
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, part)
		if len(out) >= 4 {
			break
		}
	}
	if len(out) == 0 {
		return truncateThinking(raw, 220)
	}
	return strings.Join(out, "\n")
}

func splitThinkingClauses(raw string) []string {
	replacer := strings.NewReplacer(
		"。", "\n",
		"！", "\n",
		"？", "\n",
		";", "\n",
		"；", "\n",
		"|", "\n",
	)
	raw = replacer.Replace(raw)
	items := strings.Split(raw, "\n")
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func normalizeThinkingClause(value string) string {
	value = summarySpaceRe.ReplaceAllString(strings.TrimSpace(value), " ")
	if value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) > 80 {
		value = truncateThinking(value, 80)
	}
	if utf8.RuneCountInString(value) < 3 {
		return ""
	}
	return value
}

func truncateThinking(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return strings.TrimSpace(value)
	}
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:limit])) + "..."
}
