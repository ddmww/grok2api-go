package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
	"github.com/ddmww/grok2api-go/internal/testutil"
	"github.com/gin-gonic/gin"
)

func TestOpenAIRoutes(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACCOUNT_STORAGE", "local")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNT_LOCAL_PATH", filepath.Join(dataDir, "accounts.db"))
	if err := paths.EnsureRuntimeDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "files", "images", "sample.png"), []byte("png"), 0o644); err != nil {
		t.Fatalf("write image cache: %v", err)
	}

	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.UpsertAccounts(context.Background(), []account.Upsert{
		{Token: "token-1", Pool: "basic"},
		{Token: "token-super", Pool: "super"},
	}); err != nil {
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
		Refresh: nil,
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

	t.Run("public image file", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files/image?id=sample", nil)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		if contentType := resp.Header().Get("Content-Type"); !strings.Contains(contentType, "image/png") {
			t.Fatalf("unexpected content-type: %s", contentType)
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

	t.Run("chat completions model response fallback", func(t *testing.T) {
		body := map[string]any{
			"model": "grok-4.20-fast",
			"messages": []map[string]any{
				{"role": "user", "content": "model_response_only"},
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
			t.Fatalf("model response fallback mismatch: %s", resp.Body.String())
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
		resp := testutil.NewCloseNotifyRecorder()
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
		if resp.Code != http.StatusForbidden || !strings.Contains(resp.Body.String(), `"type":"upstream_blocked"`) || !strings.Contains(resp.Body.String(), `"code":"upstream_blocked"`) {
			t.Fatalf("chat blocker mismatch: %d %s", resp.Code, resp.Body.String())
		}

		body = map[string]any{
			"model": "grok-4.20-fast",
			"input": "hello",
		}
		payload, _ = json.Marshal(body)
		req = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp = testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden || !strings.Contains(resp.Body.String(), `"type":"upstream_blocked"`) {
			t.Fatalf("responses blocker mismatch: %d %s", resp.Code, resp.Body.String())
		}

		toolBody := map[string]any{
			"model": "grok-4.20-fast",
			"messages": []map[string]any{
				{"role": "user", "content": "call_tool"},
			},
			"tools": []map[string]any{{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup_weather",
					"description": "lookup weather",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"city": map[string]any{"type": "string"}},
					},
				},
			}},
		}
		payload, _ = json.Marshal(toolBody)
		req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp = testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"tool_calls"`) {
			t.Fatalf("tool response should bypass blocker: %d %s", resp.Code, resp.Body.String())
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
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.Code)
		}
		bodyText := resp.Body.String()
		if !strings.Contains(bodyText, "response.completed") || !strings.Contains(bodyText, "[DONE]") {
			t.Fatalf("responses stream mismatch: %s", bodyText)
		}
	})

	t.Run("image generation", func(t *testing.T) {
		fake.AppChatImageMode = "final"
		fake.WebsocketImageMode = "final"
		body := map[string]any{
			"model":           "grok-imagine-image-lite",
			"prompt":          "generate image",
			"n":               1,
			"response_format": "url",
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), `"data"`) {
			t.Fatalf("unexpected body: %s", resp.Body.String())
		}
	})

	t.Run("image generation partial fallback", func(t *testing.T) {
		fake.AppChatImageMode = "preview"
		fake.WebsocketImageMode = "partial"
		body := map[string]any{
			"model":           "grok-imagine-image-lite",
			"prompt":          "generate image",
			"n":               1,
			"response_format": "url",
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), "partial.png") && !strings.Contains(resp.Body.String(), "files/image") {
			t.Fatalf("expected partial image fallback, got: %s", resp.Body.String())
		}
	})

	t.Run("image generation rate limited marks cooling", func(t *testing.T) {
		fake.AppChatImageMode = "rate_limit"
		fake.WebsocketImageMode = "final"
		body := map[string]any{
			"model":           "grok-imagine-image-lite",
			"prompt":          "generate image",
			"n":               1,
			"response_format": "url",
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		records, err := repo.GetAccounts(context.Background(), []string{"token-1"})
		if err != nil || len(records) != 1 {
			t.Fatalf("get token after rate limit fallback failed: %v %#v", err, records)
		}
		if records[0].Status != account.StatusCooling {
			t.Fatalf("expected token cooling after 429 fallback, got: %#v", records[0])
		}
		_, err = repo.PatchAccounts(context.Background(), []account.Patch{{
			Token:       "token-1",
			Status:      func() *account.Status { status := account.StatusActive; return &status }(),
			StateReason: func() *string { reason := ""; return &reason }(),
			Quota: map[string]account.QuotaWindow{
				"fast": account.DefaultQuotaSet("basic").Fast,
			},
			ExtMerge: map[string]any{"cooldown_until": int64(0)},
		}})
		if err != nil {
			t.Fatalf("reset token after rate limit fallback: %v", err)
		}
		if err := runtime.Sync(context.Background()); err != nil {
			t.Fatalf("sync runtime after reset: %v", err)
		}
	})

	t.Run("image edits", func(t *testing.T) {
		fake.ImageEditMode = "final"
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		_ = writer.WriteField("model", "grok-imagine-image-edit")
		_ = writer.WriteField("prompt", "edit image")
		fileWriter, err := writer.CreateFormFile("image[]", "sample.png")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		_, _ = fileWriter.Write([]byte("fake-image"))
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", writer.FormDataContentType())
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
	})

	t.Run("image edit 429 marks cooling", func(t *testing.T) {
		fake.ImageEditMode = "rate_limit"
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		_ = writer.WriteField("model", "grok-imagine-image-edit")
		_ = writer.WriteField("prompt", "edit image")
		fileWriter, err := writer.CreateFormFile("image[]", "sample.png")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		_, _ = fileWriter.Write([]byte("fake-image"))
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", writer.FormDataContentType())
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusTooManyRequests {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		records, err := repo.GetAccounts(context.Background(), []string{"token-super"})
		if err != nil || len(records) != 1 {
			t.Fatalf("get super token after image edit 429 failed: %v %#v", err, records)
		}
		if records[0].Status != account.StatusCooling {
			t.Fatalf("expected super token cooling after image edit 429, got: %#v", records[0])
		}
		_, err = repo.PatchAccounts(context.Background(), []account.Patch{{
			Token:       "token-super",
			Status:      func() *account.Status { status := account.StatusActive; return &status }(),
			StateReason: func() *string { reason := ""; return &reason }(),
			Quota: map[string]account.QuotaWindow{
				"auto": account.DefaultQuotaSet("super").Auto,
			},
			ExtMerge: map[string]any{"cooldown_until": int64(0)},
		}})
		if err != nil {
			t.Fatalf("reset super token after image edit 429: %v", err)
		}
		if err := runtime.Sync(context.Background()); err != nil {
			t.Fatalf("sync runtime after super reset: %v", err)
		}
	})

	t.Run("videos", func(t *testing.T) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		_ = writer.WriteField("model", "grok-imagine-video")
		_ = writer.WriteField("prompt", "make video")
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/videos", body)
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", writer.FormDataContentType())
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		id, _ := payload["id"].(string)
		if id == "" {
			t.Fatalf("missing video id: %s", resp.Body.String())
		}

		getReq := httptest.NewRequest(http.MethodGet, "/v1/videos/"+id, nil)
		getReq.Header.Set("Authorization", "Bearer test-api-key")
		getResp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(getResp, getReq)
		if getResp.Code != http.StatusOK {
			t.Fatalf("unexpected video retrieve status: %d body=%s", getResp.Code, getResp.Body.String())
		}
	})

	t.Run("chat media routing", func(t *testing.T) {
		fake.AppChatImageMode = "preview"
		fake.WebsocketImageMode = "partial"
		body := map[string]any{
			"model": "grok-imagine-image-lite",
			"messages": []map[string]any{
				{"role": "user", "content": "generate image"},
			},
		}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-api-key")
		req.Header.Set("Content-Type", "application/json")
		resp := testutil.NewCloseNotifyRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), "partial") && !strings.Contains(resp.Body.String(), "files/image") {
			t.Fatalf("unexpected media body: %s", resp.Body.String())
		}
	})
}
