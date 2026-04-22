package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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

func TestMessagesRoute(t *testing.T) {
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
		"app":      map[string]any{"api_key": "test-api-key"},
		"features": map[string]any{"temporary": true, "memory": false, "thinking": true, "stream": true},
		"proxy": map[string]any{
			"egress":   map[string]any{"mode": "direct"},
			"upstream": map[string]any{"base_url": fake.BaseURL()},
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

	t.Run("non-stream", func(t *testing.T) {
		body := map[string]any{
			"model":  "grok-4.20-fast",
			"stream": false,
			"messages": []map[string]any{
				{"role": "user", "content": "hello"},
			},
		}
		payload, _ := json.Marshal(body)
		req := testutil.NewCloseNotifyRecorder()
		request, _ := http.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-api-key")
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(req, request)
		if req.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", req.Code)
		}
		if !strings.Contains(req.Body.String(), `"type":"message"`) {
			t.Fatalf("unexpected body: %s", req.Body.String())
		}
	})

	t.Run("stream", func(t *testing.T) {
		body := map[string]any{
			"model":  "grok-4.20-fast",
			"stream": true,
			"messages": []map[string]any{
				{"role": "user", "content": "hello"},
			},
		}
		payload, _ := json.Marshal(body)
		req := testutil.NewCloseNotifyRecorder()
		request, _ := http.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-api-key")
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(req, request)
		if req.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", req.Code)
		}
		bodyText := req.Body.String()
		if !strings.Contains(bodyText, "message_start") || !strings.Contains(bodyText, "message_stop") {
			t.Fatalf("unexpected stream body: %s", bodyText)
		}
	})

	t.Run("auto mode falls back to fast quota", func(t *testing.T) {
		status := account.StatusActive
		reason := ""
		lastUseAt := int64(0)
		if _, err := repo.PatchAccounts(context.Background(), []account.Patch{{
			Token:       "token-1",
			Status:      &status,
			StateReason: &reason,
			LastUseAt:   &lastUseAt,
			Quota: map[string]account.QuotaWindow{
				"auto": account.QuotaWindow{Remaining: 0, Total: 20, WindowSeconds: 72000},
				"fast": account.DefaultQuotaSet("basic").Fast,
			},
			ExtMerge: map[string]any{"cooldown_until": int64(0)},
		}}); err != nil {
			t.Fatalf("patch fallback quotas: %v", err)
		}
		if err := runtime.Sync(context.Background()); err != nil {
			t.Fatalf("sync runtime after fallback quota patch: %v", err)
		}

		body := map[string]any{
			"model":  "grok-4.20-auto",
			"stream": false,
			"messages": []map[string]any{
				{"role": "user", "content": "hello"},
			},
		}
		payload, _ := json.Marshal(body)
		req := testutil.NewCloseNotifyRecorder()
		request, _ := http.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-api-key")
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(req, request)
		if req.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", req.Code, req.Body.String())
		}
		if !strings.Contains(req.Body.String(), `"type":"message"`) {
			t.Fatalf("unexpected body: %s", req.Body.String())
		}
	})

	t.Run("non-stream upstream blocker", func(t *testing.T) {
		if err := cfg.Update(context.Background(), map[string]any{
			"upstream_blocker": map[string]any{
				"enabled":        true,
				"case_sensitive": false,
				"keywords":       []string{"fake grok"},
				"message":        "blocked by upstream",
			},
		}); err != nil {
			t.Fatalf("update config: %v", err)
		}

		body := map[string]any{
			"model":  "grok-4.20-fast",
			"stream": false,
			"messages": []map[string]any{
				{"role": "user", "content": "hello"},
			},
		}
		payload, _ := json.Marshal(body)
		req := testutil.NewCloseNotifyRecorder()
		request, _ := http.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-api-key")
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(req, request)
		if req.Code != http.StatusForbidden || !strings.Contains(req.Body.String(), `"type":"upstream_blocked"`) {
			t.Fatalf("unexpected blocker response: %d %s", req.Code, req.Body.String())
		}

		toolBody := map[string]any{
			"model":  "grok-4.20-fast",
			"stream": false,
			"messages": []map[string]any{
				{"role": "user", "content": "call_tool"},
			},
			"tools": []map[string]any{{
				"name":        "lookup_weather",
				"description": "lookup weather",
				"input_schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
				},
			}},
		}
		payload, _ = json.Marshal(toolBody)
		req = testutil.NewCloseNotifyRecorder()
		request, _ = http.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-api-key")
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(req, request)
		if req.Code != http.StatusOK || !strings.Contains(req.Body.String(), `"type":"tool_use"`) {
			t.Fatalf("tool use should bypass blocker: %d %s", req.Code, req.Body.String())
		}
	})
}
