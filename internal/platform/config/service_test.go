package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceLoadAndUpdate(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(dataDir, "config.toml")

	t.Setenv("APP_BASE_DIR", root)
	t.Setenv("DATA_DIR", dataDir)

	if err := os.WriteFile(filepath.Join(root, "config.defaults.toml"), []byte("[app]\napp_key='default-admin'\napi_key='default-api'\n[proxy.egress]\nmode='direct'\n"), 0o644); err != nil {
		t.Fatalf("write defaults: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("[app]\napi_key='override-api'\n[proxy.egress]\nmode='single_proxy'\nproxy_url='http://127.0.0.1:7897'\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	service := New(NewLocalBackend(configPath))
	if err := service.Load(context.Background()); err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got := service.GetString("app.app_key", ""); got != "default-admin" {
		t.Fatalf("app_key mismatch: %q", got)
	}
	if got := service.GetString("app.api_key", ""); got != "override-api" {
		t.Fatalf("api_key mismatch: %q", got)
	}
	if got := service.GetString("proxy.egress.mode", ""); got != "single_proxy" {
		t.Fatalf("proxy mode mismatch: %q", got)
	}

	if err := service.Update(context.Background(), map[string]any{
		"app": map[string]any{"app_key": "updated-admin"},
	}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	reloaded := New(NewLocalBackend(configPath))
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := reloaded.GetString("app.app_key", ""); got != "updated-admin" {
		t.Fatalf("updated app_key mismatch: %q", got)
	}
	if got := reloaded.GetString("app.api_key", ""); got != "override-api" {
		t.Fatalf("updated api_key mismatch: %q", got)
	}
}
