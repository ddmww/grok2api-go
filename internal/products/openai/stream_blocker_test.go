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
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
	"github.com/ddmww/grok2api-go/internal/testutil"
	"github.com/gin-gonic/gin"
)

func TestChatStreamUpstreamBlockerUsesRetryBuffer(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))
	if err := paths.EnsureRuntimeDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}

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
	fake.ChatContent = "prefix blocked phrase suffix"

	cfg := testutil.NewConfig(map[string]any{
		"app": map[string]any{
			"api_key": "test-api-key",
		},
		"features": map[string]any{
			"temporary": true,
			"memory":    false,
			"thinking":  false,
		},
		"chat": map[string]any{
			"stream_retry_enabled":      true,
			"stream_retry_buffer_runes": 320,
		},
		"upstream_blocker": map[string]any{
			"enabled":        true,
			"case_sensitive": false,
			"keywords":       []string{"blocked phrase"},
			"message":        "blocked by upstream",
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

	body := map[string]any{
		"model":  "grok-4.20-fast",
		"stream": true,
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
	bodyText := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("stream should keep SSE status 200: %d %s", resp.Code, bodyText)
	}
	if !strings.Contains(bodyText, `"type":"upstream_blocked"`) || !strings.Contains(bodyText, `"code":"upstream_blocked"`) {
		t.Fatalf("stream blocker mismatch: %s", bodyText)
	}
	if strings.Contains(bodyText, "blocked phrase suffix") {
		t.Fatalf("blocked content leaked: %s", bodyText)
	}
	if !strings.Contains(bodyText, "data: [DONE]") {
		t.Fatalf("stream should terminate with DONE: %s", bodyText)
	}
}
