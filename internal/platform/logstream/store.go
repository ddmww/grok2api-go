package logstream

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const DefaultCapacity = 1000

type Category string

const (
	CategoryChat   Category = "chat"
	CategoryImage  Category = "image"
	CategoryError  Category = "error"
	CategorySystem Category = "system"
)

type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

type Event struct {
	ID           int64    `json:"id"`
	Time         string   `json:"time"`
	UnixMillis   int64    `json:"unix_ms"`
	Category     Category `json:"category"`
	Level        Level    `json:"level"`
	Path         string   `json:"path,omitempty"`
	Model        string   `json:"model,omitempty"`
	StatusCode   int      `json:"status_code,omitempty"`
	DurationMS   int64    `json:"duration_ms,omitempty"`
	SSO          string   `json:"sso,omitempty"`
	ErrorSummary string   `json:"error_summary,omitempty"`
	Message      string   `json:"message"`
}

type Query struct {
	Category Category
	Level    Level
	Limit    int
}

type Store struct {
	mu          sync.RWMutex
	nextID      int64
	capacity    int
	events      []Event
	subscribers map[int64]chan Event
	nextSubID   int64
}

func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Store{
		capacity:    capacity,
		events:      make([]Event, 0, capacity),
		subscribers: map[int64]chan Event{},
	}
}

func (s *Store) Add(event Event) Event {
	if s == nil {
		return event
	}
	now := time.Now()
	if event.UnixMillis == 0 {
		event.UnixMillis = now.UnixMilli()
	}
	if strings.TrimSpace(event.Time) == "" {
		event.Time = time.UnixMilli(event.UnixMillis).Format(time.RFC3339Nano)
	}
	if event.Category == "" {
		event.Category = CategorySystem
	}
	if event.Level == "" {
		event.Level = LevelInfo
	}
	event.Message = strings.TrimSpace(event.Message)
	event.ErrorSummary = trimSummary(event.ErrorSummary)

	s.mu.Lock()
	s.nextID++
	event.ID = s.nextID
	if len(s.events) >= s.capacity {
		copy(s.events, s.events[1:])
		s.events[len(s.events)-1] = event
	} else {
		s.events = append(s.events, event)
	}
	subscribers := make([]chan Event, 0, len(s.subscribers))
	for _, ch := range s.subscribers {
		subscribers = append(subscribers, ch)
	}
	s.mu.Unlock()

	for _, ch := range subscribers {
		select {
		case ch <- event:
		default:
		}
	}
	return event
}

func (s *Store) List(query Query) []Event {
	if s == nil {
		return nil
	}
	if query.Limit <= 0 || query.Limit > s.capacity {
		query.Limit = s.capacity
	}
	s.mu.RLock()
	copied := make([]Event, 0, len(s.events))
	for _, event := range s.events {
		if !matches(event, query) {
			continue
		}
		copied = append(copied, event)
	}
	s.mu.RUnlock()
	if len(copied) > query.Limit {
		copied = copied[len(copied)-query.Limit:]
	}
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].ID > copied[j].ID
	})
	return copied
}

func (s *Store) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.events = s.events[:0]
	s.mu.Unlock()
}

func (s *Store) Subscribe(query Query) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	if s == nil {
		return ch, func() { close(ch) }
	}
	s.mu.Lock()
	s.nextSubID++
	id := s.nextSubID
	s.subscribers[id] = ch
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		if existing, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(existing)
		}
		s.mu.Unlock()
	}
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		defer cancel()
		for event := range ch {
			if !matches(event, query) {
				continue
			}
			select {
			case out <- event:
			default:
			}
		}
	}()
	return out, cancel
}

func matches(event Event, query Query) bool {
	if query.Category != "" && event.Category != query.Category {
		return false
	}
	if query.Level != "" && event.Level != query.Level {
		return false
	}
	return true
}

func trimSummary(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) <= 300 {
		return value
	}
	return value[:300] + "..."
}

func NormalizeCategory(raw string) Category {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(CategoryChat):
		return CategoryChat
	case string(CategoryImage):
		return CategoryImage
	case string(CategoryError):
		return CategoryError
	case string(CategorySystem):
		return CategorySystem
	default:
		return ""
	}
}

func NormalizeLevel(raw string) Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(LevelInfo):
		return LevelInfo
	case "warning", string(LevelWarn):
		return LevelWarn
	case string(LevelError):
		return LevelError
	default:
		return ""
	}
}

func MaskSSO(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "sso=") {
		value = strings.TrimPrefix(value, "sso=")
	}
	if len(value) <= 10 {
		return value[:min(len(value), 4)] + "****"
	}
	return value[:6] + "..." + value[len(value)-4:]
}
