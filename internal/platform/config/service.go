package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/logging"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/pelletier/go-toml/v2"
	"gorm.io/gorm"
)

type Backend interface {
	Load(context.Context) (map[string]any, error)
	Save(context.Context, map[string]any) error
}

type Service struct {
	mu      sync.RWMutex
	backend Backend
	values  map[string]any
}

type configState struct {
	ID        uint   `gorm:"primaryKey"`
	Payload   string `gorm:"type:longtext"`
	UpdatedAt int64  `gorm:"column:updated_at"`
}

func NewLocalBackend(path string) Backend {
	return &localBackend{path: path}
}

func NewMySQLBackend(db *gorm.DB) Backend {
	return &mysqlBackend{db: db}
}

func New(backend Backend) *Service {
	return &Service{backend: backend, values: map[string]any{}}
}

func (s *Service) Load(ctx context.Context) error {
	defaults, err := readTOML(paths.ConfigDefaultsPath())
	if err != nil {
		return err
	}
	current := deepClone(defaults)
	if s.backend != nil {
		overrides, err := s.backend.Load(ctx)
		if err != nil {
			return err
		}
		deepMerge(current, overrides)
	}
	s.mu.Lock()
	s.values = current
	s.mu.Unlock()
	return nil
}

func (s *Service) Update(ctx context.Context, patch map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := deepClone(s.values)
	deepMerge(next, patch)
	if s.backend == nil {
		return errors.New("config backend is not initialized")
	}
	if err := s.backend.Save(ctx, next); err != nil {
		return err
	}
	s.values = next
	return nil
}

func (s *Service) Raw() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return deepClone(s.values)
}

func (s *Service) Get(path string) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getPath(s.values, path)
}

func (s *Service) GetString(path, fallback string) string {
	value := s.Get(path)
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return v
	case fmt.Stringer:
		return v.String()
	default:
		if value == nil {
			return fallback
		}
		return fmt.Sprint(value)
	}
}

func (s *Service) GetBool(path string, fallback bool) bool {
	value := s.Get(path)
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		default:
			return fallback
		}
	default:
		return fallback
	}
}

func (s *Service) GetInt(path string, fallback int) int {
	value := s.Get(path)
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

func (s *Service) GetFloat(path string, fallback float64) float64 {
	value := s.Get(path)
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%f", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

func (s *Service) GetStringSlice(path string) []string {
	value := s.Get(path)
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text := strings.TrimSpace(fmt.Sprint(item))
			if text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

type localBackend struct {
	path string
}

func (b *localBackend) Load(_ context.Context) (map[string]any, error) {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(b.path); errors.Is(err, os.ErrNotExist) {
		defaultsPath := paths.ConfigDefaultsPath()
		data, readErr := os.ReadFile(defaultsPath)
		if readErr != nil {
			return nil, readErr
		}
		if writeErr := os.WriteFile(b.path, data, 0o644); writeErr != nil {
			return nil, writeErr
		}
	}
	return readTOML(b.path)
}

func (b *localBackend) Save(_ context.Context, values map[string]any) error {
	data, err := toml.Marshal(values)
	if err != nil {
		return err
	}
	return os.WriteFile(b.path, data, 0o644)
}

type mysqlBackend struct {
	db *gorm.DB
}

func (b *mysqlBackend) Load(ctx context.Context) (map[string]any, error) {
	if err := b.db.WithContext(ctx).AutoMigrate(&configState{}); err != nil {
		return nil, err
	}
	var state configState
	if err := b.db.WithContext(ctx).First(&state, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			localPath := paths.LocalConfigPath()
			if _, statErr := os.Stat(localPath); statErr == nil {
				values, loadErr := readTOML(localPath)
				if loadErr != nil {
					return nil, loadErr
				}
				if saveErr := b.Save(ctx, values); saveErr != nil {
					return nil, saveErr
				}
				return values, nil
			}
			return map[string]any{}, nil
		}
		return nil, err
	}
	values := map[string]any{}
	if strings.TrimSpace(state.Payload) == "" {
		return values, nil
	}
	if err := toml.Unmarshal([]byte(state.Payload), &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (b *mysqlBackend) Save(ctx context.Context, values map[string]any) error {
	if err := b.db.WithContext(ctx).AutoMigrate(&configState{}); err != nil {
		return err
	}
	data, err := toml.Marshal(values)
	if err != nil {
		return err
	}
	state := configState{
		ID:        1,
		Payload:   string(data),
		UpdatedAt: time.Now().UnixMilli(),
	}
	return b.db.WithContext(ctx).Save(&state).Error
}

func readTOML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if err := toml.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func deepClone(values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = deepClone(typed)
		case []any:
			copied := make([]any, len(typed))
			copy(copied, typed)
			out[key] = copied
		default:
			out[key] = value
		}
	}
	return out
}

func deepMerge(dst map[string]any, src map[string]any) {
	for key, value := range src {
		child, ok := value.(map[string]any)
		if !ok {
			dst[key] = value
			continue
		}
		existing, ok := dst[key].(map[string]any)
		if !ok {
			dst[key] = deepClone(child)
			continue
		}
		deepMerge(existing, child)
	}
}

func getPath(values map[string]any, path string) any {
	current := any(values)
	for _, part := range strings.Split(path, ".") {
		node, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		next, ok := node[part]
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func LogLoaded(service *Service) {
	logging.L().Info("config loaded", "storage", os.Getenv("ACCOUNT_STORAGE"), "data_dir", paths.DataDir())
}
