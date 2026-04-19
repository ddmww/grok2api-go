package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestAdminRoutes(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	dataDir := filepath.Join(t.TempDir(), "data")
	imageDir := filepath.Join(dataDir, "files", "images")

	t.Setenv("APP_BASE_DIR", root)
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))

	if err := paths.EnsureRuntimeDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "sample.png"), []byte("png"), 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}

	fake := testutil.NewFakeGrokServer()
	defer fake.Close()

	cfg := testutil.NewConfig(map[string]any{
		"app": map[string]any{
			"app_key": "admin-key",
			"api_key": "test-api-key",
		},
		"features": map[string]any{
			"temporary": true,
			"memory":    false,
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
	refreshService := refresh.New(repo, runtime, cfg, xaiClient)
	state := &app.State{
		Config:  cfg,
		Repo:    repo,
		Runtime: runtime,
		Refresh: refreshService,
		Proxy:   proxyRuntime,
		XAI:     xaiClient,
		Tasks:   tasks.NewStore(),
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	Mount(router, state)

	doJSON := func(method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var reader *bytes.Reader
		if body == nil {
			reader = bytes.NewReader(nil)
		} else {
			payload, _ := json.Marshal(body)
			reader = bytes.NewReader(payload)
		}
		req := httptest.NewRequest(method, target, reader)
		req.Header.Set("Authorization", "Bearer admin-key")
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}

	resp := doJSON(http.MethodGet, "/admin/api/status", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"status":"ok"`) {
		t.Fatalf("status route failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode":      "single_proxy",
				"proxy_url": "http://127.0.0.1:7897",
			},
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config update failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/tokens", map[string]any{
		"basic": []map[string]any{{"token": "token-1", "tags": []string{"seed"}}},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("tokens replace failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/tokens", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "token-1") {
		t.Fatalf("tokens list failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/cache", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "sample.png") && !strings.Contains(resp.Body.String(), "local_image") {
		t.Fatalf("cache route failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/batch/refresh?async=true", map[string]any{
		"tokens": []string{"token-1"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("batch create failed: %d %s", resp.Code, resp.Body.String())
	}
	var taskPayload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &taskPayload); err != nil {
		t.Fatalf("unmarshal task payload: %v", err)
	}
	taskID, _ := taskPayload["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task id: %s", resp.Body.String())
	}

	time.Sleep(100 * time.Millisecond)
	streamReq := httptest.NewRequest(http.MethodGet, "/admin/api/batch/"+taskID+"/stream", nil)
	streamReq.Header.Set("Authorization", "Bearer admin-key")
	streamResp := httptest.NewRecorder()
	router.ServeHTTP(streamResp, streamReq)
	if streamResp.Code != http.StatusOK || !strings.Contains(streamResp.Body.String(), "done") {
		t.Fatalf("batch stream failed: %d %s", streamResp.Code, streamResp.Body.String())
	}
}
