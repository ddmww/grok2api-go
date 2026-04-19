package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/control/refresh"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
	"github.com/ddmww/grok2api-go/internal/testutil"
	"github.com/gin-gonic/gin"
)

func TestOpenAIRoutes(t *testing.T) {
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(t.TempDir(), "accounts.db"))

	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(context.Background(), []account.Upsert{{Token: "token-1", Pool: "basic"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	fake := testutil.NewFakeGrokServer()
	defer fake.Close()

	cfg := testutil.NewConfig(map[string]any{
		"app": map[string]any{
			"api_key": "test-api-key",
			"app_key": "admin-key",
		},
		"features": map[string]any{
			"temporary": true,
			"memory":    false,
			"thinking":  true,
		},
		"proxy": map[string]any{
			"egress": map[string]any{"mode": "direct"},
			"upstream": map[string]any{
				"base_url": fake.BaseURL(),
			},
		},
	})

	runtime := account.NewRuntime(repo)
	if err := runtime.Sync(context.Background()); err != nil {
		t.Fatalf("sync runtime: %v", err)
	}
	proxyRuntime := proxy.NewRuntime(cfg)
	xaiClient := xai.NewClient(cfg, proxyRuntime)
	state := &app.State{
		Config:  cfg,
		Repo:    repo,
		Runtime: runtime,
		Refresh: refresh.New(repo, runtime, cfg, xaiClient),
		Proxy:   proxyRuntime,
		XAI:     xaiClient,
		Tasks:   tasks.NewStore(),
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	Mount(router, state)

	t.Run("models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-api-key")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.Code)
		}
		if !strings.Contains(resp.Body.String(), "grok-4.20-fast") {
			t.Fatalf("models body missing expected model: %s", resp.Body.String())
		}
	})

	t.Run("chat completions", func(t *testing.T) {
		body := map[string]any{
			"model": "grok-4.20-fast",
			"messages": []map[string]any{
				{"role": "user", "content": "hello"},
			},
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.Code)
		}
		if !strings.Contains(resp.Body.String(), "Hello from fake grok") {
			t.Fatalf("chat body mismatch: %s", resp.Body.String())
		}
	})

	t.Run("chat completions stream", func(t *testing.T) {
		body := map[string]any{
			"model":    "grok-4.20-fast",
			"stream":   true,
			"messages": []map[string]any{{"role": "user", "content": "hello"}},
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.Code)
		}
		bodyText := resp.Body.String()
		if !strings.Contains(bodyText, "chat.completion.chunk") || !strings.Contains(bodyText, "[DONE]") {
			t.Fatalf("stream body mismatch: %s", bodyText)
		}
	})

	t.Run("responses", func(t *testing.T) {
		body := map[string]any{
			"model": "grok-4.20-fast",
			"input": "hello",
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.Code)
		}
		if !strings.Contains(resp.Body.String(), `"object":"response"`) {
			t.Fatalf("responses body mismatch: %s", resp.Body.String())
		}
	})

	t.Run("responses stream", func(t *testing.T) {
		body := map[string]any{
			"model":  "grok-4.20-fast",
			"input":  "hello",
			"stream": true,
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.Code)
		}
		bodyText := resp.Body.String()
		if !strings.Contains(bodyText, "response.completed") || !strings.Contains(bodyText, "[DONE]") {
			t.Fatalf("responses stream mismatch: %s", bodyText)
		}
	})
}
