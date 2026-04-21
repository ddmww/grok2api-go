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
	"github.com/ddmww/grok2api-go/internal/platform/updatecheck"
	"github.com/ddmww/grok2api-go/internal/testutil"
	"github.com/gin-gonic/gin"
)

type fakeUpdateService struct {
	info updatecheck.ReleaseInfo
}

func (f fakeUpdateService) GetLatestReleaseInfo(context.Context, bool) updatecheck.ReleaseInfo {
	return f.info
}

func intPtr(value int) *int                          { return &value }
func int64Ptr(value int64) *int64                    { return &value }
func stringPtr(value string) *string                 { return &value }
func statusPtr(value account.Status) *account.Status { return &value }

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
	previousUpdateService := newUpdateService
	newUpdateService = func() updateService {
		return fakeUpdateService{info: updatecheck.ReleaseInfo{
			Status:          "ok",
			CurrentVersion:  "0.1.0-dev",
			LatestVersion:   "0.2.0",
			HasUpdate:       true,
			UpdateAvailable: true,
			ReleaseNotes:    "stable notes",
			ReleaseURL:      "https://example.com/release",
			PublishedAt:     "2026-04-20T00:00:00Z",
		}}
	}
	defer func() { newUpdateService = previousUpdateService }()

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

	metaResp := httptest.NewRecorder()
	metaReq := httptest.NewRequest(http.MethodGet, "/meta/update?force=true", nil)
	router.ServeHTTP(metaResp, metaReq)
	if metaResp.Code != http.StatusOK || !strings.Contains(metaResp.Body.String(), `"update_available":true`) {
		t.Fatalf("meta update failed: %d %s", metaResp.Code, metaResp.Body.String())
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

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config reset failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"proxy": map[string]any{
			"cf_clearance": " abc \u200d 123 ",
			"user_agent":   " test\u2014agent ",
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config sanitize failed: %d %s", resp.Code, resp.Body.String())
	}
	rawConfig := cfg.Raw()
	proxySection, _ := rawConfig["proxy"].(map[string]any)
	if proxySection["cf_clearance"] != "abc123" || proxySection["user_agent"] != "test-agent" {
		t.Fatalf("config sanitize mismatch: %#v", proxySection)
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"upstream_blocker": map[string]any{
			"enabled":        true,
			"case_sensitive": false,
			"keywords":       " foo \nbar\nfoo\n",
			"message":        " custom blocker ",
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("upstream blocker config update failed: %d %s", resp.Code, resp.Body.String())
	}
	rawConfig = cfg.Raw()
	blockerSection, _ := rawConfig["upstream_blocker"].(map[string]any)
	keywordsLen := 0
	switch keywords := blockerSection["keywords"].(type) {
	case []any:
		keywordsLen = len(keywords)
	case []string:
		keywordsLen = len(keywords)
	}
	if blockerSection["enabled"] != true || blockerSection["message"] != "custom blocker" || keywordsLen != 2 {
		t.Fatalf("upstream blocker normalize mismatch: %#v", blockerSection)
	}

	if err := cfg.Update(context.Background(), map[string]any{
		"upstream_blocker": map[string]any{
			"enabled":        "true",
			"case_sensitive": "false",
			"keywords":       " legacy-one \nlegacy-two\n",
			"message":        "<nil>",
		},
	}); err != nil {
		t.Fatalf("seed legacy upstream blocker failed: %v", err)
	}

	resp = doJSON(http.MethodGet, "/admin/api/config", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("config get with legacy upstream blocker failed: %d %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"enabled":true`) || !strings.Contains(resp.Body.String(), `legacy-one`) || !strings.Contains(resp.Body.String(), `legacy-two`) || !strings.Contains(resp.Body.String(), `上游渠道商拦截了当前请求`) {
		t.Fatalf("legacy upstream blocker not normalized on get: %s", resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"upstream_blocker": map[string]any{
			"enabled": true,
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("legacy upstream blocker partial update failed: %d %s", resp.Code, resp.Body.String())
	}
	rawConfig = cfg.Raw()
	blockerSection, _ = rawConfig["upstream_blocker"].(map[string]any)
	if blockerSection["enabled"] != true || blockerSection["message"] != "上游渠道商拦截了当前请求，请尝试换个说法后重试，或稍后再试。" {
		t.Fatalf("legacy upstream blocker partial update normalization mismatch: %#v", blockerSection)
	}
	keywordsLen = 0
	switch keywords := blockerSection["keywords"].(type) {
	case []any:
		keywordsLen = len(keywords)
	case []string:
		keywordsLen = len(keywords)
	}
	if keywordsLen != 2 {
		t.Fatalf("legacy upstream blocker keywords lost after partial update: %#v", blockerSection["keywords"])
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"upstream_blocker": map[string]any{
			"enabled":  true,
			"keywords": []string{},
			"message":  "still set",
		},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("empty upstream blocker keywords should fail: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"upstream_blocker": map[string]any{
			"enabled":  true,
			"keywords": []string{"blocked"},
			"message":  "",
		},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("empty upstream blocker message should fail: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/config", map[string]any{
		"account": map[string]any{
			"mysql": map[string]any{
				"url": "mysql://example",
			},
		},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("startup-only config should fail: %d %s", resp.Code, resp.Body.String())
	}

	pageResp := httptest.NewRecorder()
	pageReq := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	router.ServeHTTP(pageResp, pageReq)
	if pageResp.Code != http.StatusOK || strings.Contains(pageResp.Body.String(), `data-webui-link`) {
		t.Fatalf("config page should not expose webui link: %d %s", pageResp.Code, pageResp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/tokens", map[string]any{
		"basic": []map[string]any{{"token": "token-1", "tags": []string{"seed"}}},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("tokens replace failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/tokens", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "token-1") || !strings.Contains(resp.Body.String(), `"fail_count"`) {
		t.Fatalf("tokens list failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/models", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"grok-imagine-image-lite"`) || !strings.Contains(resp.Body.String(), `"grok-4.3-beta"`) || !strings.Contains(resp.Body.String(), `"mode":"static"`) {
		t.Fatalf("models list failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/tokens", map[string]any{
		"basic": []any{
			map[string]any{"token": "token-1", "tags": []string{"seed"}},
			" sso=token-2 ",
			map[string]any{"token": "token-2", "tags": []string{"dup"}},
			map[string]any{"token": "token-3", "tags": []string{"json"}},
		},
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"count":3`) {
		t.Fatalf("tokens replace sanitize failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/tokens?page=2&page_size=2", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"page":2`) || !strings.Contains(resp.Body.String(), `"page_size":2`) || !strings.Contains(resp.Body.String(), `"total_pages":2`) {
		t.Fatalf("tokens paged list failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/tokens/add", map[string]any{
		"pool":   "auto",
		"tokens": []string{" sso=token-auto ", "token-auto", "token-3"},
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"count":1`) || !strings.Contains(resp.Body.String(), `"skipped":1`) || !strings.Contains(resp.Body.String(), `"synced":true`) {
		t.Fatalf("tokens add auto failed: %d %s", resp.Code, resp.Body.String())
	}

	autoRecords, err := repo.GetAccounts(context.Background(), []string{"token-auto"})
	if err != nil || len(autoRecords) != 1 {
		t.Fatalf("get auto token failed: %v %#v", err, autoRecords)
	}
	if autoRecords[0].Pool != "basic" || autoRecords[0].UsageSyncCount == 0 {
		t.Fatalf("auto token not refreshed: %#v", autoRecords[0])
	}

	if _, err := repo.PatchAccounts(context.Background(), []account.Patch{{
		Token:       "token-auto",
		Status:      statusPtr(account.StatusCooling),
		StateReason: stringPtr("rate_limited"),
		ExtMerge:    map[string]any{"cooldown_until": account.NowMS() + 3600_000},
	}}); err != nil {
		t.Fatalf("seed cooling token failed: %v", err)
	}
	if err := runtime.Sync(context.Background()); err != nil {
		t.Fatalf("sync cooling token failed: %v", err)
	}
	if _, err := refreshService.RefreshTokens(context.Background(), []string{"token-auto"}); err != nil {
		t.Fatalf("manual refresh service failed: %v", err)
	}
	refreshedRecords, err := repo.GetAccounts(context.Background(), []string{"token-auto"})
	if err != nil || len(refreshedRecords) != 1 {
		t.Fatalf("get refreshed token failed: %v %#v", err, refreshedRecords)
	}
	if refreshedRecords[0].Status != account.StatusActive || refreshedRecords[0].StateReason != "" {
		t.Fatalf("manual refresh should recover cooling token: %#v", refreshedRecords[0])
	}

	if _, err := repo.PatchAccounts(context.Background(), []account.Patch{{
		Token:          "token-1",
		Quota:          map[string]account.QuotaWindow{"auto": {Remaining: 7, Total: 20, WindowSeconds: 3600, Source: account.QuotaSourceReal}},
		UsageUseDelta:  intPtr(3),
		UsageFailDelta: intPtr(2),
		UsageSyncDelta: intPtr(1),
		LastUseAt:      int64Ptr(111),
		LastFailAt:     int64Ptr(222),
		LastFailReason: stringPtr("bad credentials"),
		LastSyncAt:     int64Ptr(333),
		LastClearAt:    int64Ptr(444),
		StateReason:    stringPtr("token_expired"),
		ExtMerge:       map[string]any{"migrated": true},
	}}); err != nil {
		t.Fatalf("seed token patch failed: %v", err)
	}
	if _, err := repo.PatchAccounts(context.Background(), []account.Patch{{
		Token:   "token-3",
		AddTags: []string{"nsfw"},
	}}); err != nil {
		t.Fatalf("seed nsfw tag failed: %v", err)
	}

	resp = doJSON(http.MethodGet, "/admin/api/tokens/summary?pool=basic&nsfw=enabled", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"enabled":1`) || !strings.Contains(resp.Body.String(), `"basic":1`) {
		t.Fatalf("tokens summary failed: %d %s", resp.Code, resp.Body.String())
	}

	exportReqBody, _ := json.Marshal(map[string]any{
		"format": "txt",
		"filters": map[string]any{
			"pool":   "basic",
			"status": "all",
			"nsfw":   "enabled",
		},
		"select_all_filtered": true,
	})
	exportReq := httptest.NewRequest(http.MethodPost, "/admin/api/tokens/export", bytes.NewReader(exportReqBody))
	exportReq.Header.Set("Authorization", "Bearer admin-key")
	exportReq.Header.Set("Content-Type", "application/json")
	exportResp := httptest.NewRecorder()
	router.ServeHTTP(exportResp, exportReq)
	if exportResp.Code != http.StatusOK || !strings.Contains(exportResp.Body.String(), "token-3") {
		t.Fatalf("token export txt failed: %d %s", exportResp.Code, exportResp.Body.String())
	}
	if !strings.Contains(exportResp.Header().Get("Content-Disposition"), ".txt") {
		t.Fatalf("token export txt filename missing: %s", exportResp.Header().Get("Content-Disposition"))
	}

	exportReqBody, _ = json.Marshal(map[string]any{
		"format": "json",
		"tokens": []string{"token-3"},
	})
	exportReq = httptest.NewRequest(http.MethodPost, "/admin/api/tokens/export", bytes.NewReader(exportReqBody))
	exportReq.Header.Set("Authorization", "Bearer admin-key")
	exportReq.Header.Set("Content-Type", "application/json")
	exportResp = httptest.NewRecorder()
	router.ServeHTTP(exportResp, exportReq)
	if exportResp.Code != http.StatusOK || !strings.Contains(exportResp.Body.String(), `"token":"token-3"`) {
		t.Fatalf("token export json failed: %d %s", exportResp.Code, exportResp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/batch/nsfw", map[string]any{
		"tokens": []string{"token-2"},
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"ok":1`) {
		t.Fatalf("batch nsfw failed: %d %s", resp.Code, resp.Body.String())
	}
	nsfwRecords, err := repo.GetAccounts(context.Background(), []string{"token-2"})
	if err != nil || len(nsfwRecords) != 1 {
		t.Fatalf("get nsfw token failed: %v %#v", err, nsfwRecords)
	}
	foundNSFW := false
	for _, tag := range nsfwRecords[0].Tags {
		if tag == "nsfw" {
			foundNSFW = true
			break
		}
	}
	if !foundNSFW {
		t.Fatalf("batch nsfw should add nsfw tag: %#v", nsfwRecords[0])
	}

	sourceRecords, err := repo.GetAccounts(context.Background(), []string{"token-1"})
	if err != nil || len(sourceRecords) != 1 {
		t.Fatalf("get source token failed: %v %#v", err, sourceRecords)
	}
	source := sourceRecords[0]

	resp = doJSON(http.MethodPut, "/admin/api/tokens/edit", map[string]any{
		"old_token": "token-1",
		"token":     "token-1-renamed",
		"pool":      "super",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("token edit failed: %d %s", resp.Code, resp.Body.String())
	}

	editedRecords, err := repo.GetAccounts(context.Background(), []string{"token-1-renamed"})
	if err != nil || len(editedRecords) != 1 {
		t.Fatalf("get edited token failed: %v %#v", err, editedRecords)
	}
	edited := editedRecords[0]
	if edited.Pool != "super" || edited.UsageUseCount != source.UsageUseCount || edited.UsageFailCount != source.UsageFailCount || edited.UsageSyncCount != source.UsageSyncCount {
		t.Fatalf("edited token usage mismatch: %#v", edited)
	}
	if edited.LastUseAt != source.LastUseAt || edited.LastFailAt != source.LastFailAt || edited.LastFailReason != source.LastFailReason || edited.LastSyncAt != source.LastSyncAt || edited.LastClearAt != source.LastClearAt {
		t.Fatalf("edited token timestamps mismatch: %#v", edited)
	}
	if edited.StateReason != source.StateReason || edited.Ext["migrated"] != true {
		t.Fatalf("edited token state mismatch: %#v", edited)
	}
	if edited.Quota.Auto.Remaining != source.Quota.Auto.Remaining || edited.Quota.Auto.Total != source.Quota.Auto.Total {
		t.Fatalf("edited token quota mismatch: %#v", edited.Quota)
	}

	resp = doJSON(http.MethodPut, "/admin/api/tokens/edit", map[string]any{
		"old_token": "token-1-renamed",
		"token":     "token-1-renamed",
		"pool":      "heavy",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("token pool edit failed: %d %s", resp.Code, resp.Body.String())
	}

	editedRecords, err = repo.GetAccounts(context.Background(), []string{"token-1-renamed"})
	if err != nil || len(editedRecords) != 1 {
		t.Fatalf("get edited token after pool change failed: %v %#v", err, editedRecords)
	}
	if editedRecords[0].Pool != "heavy" || editedRecords[0].UsageUseCount != source.UsageUseCount || editedRecords[0].Quota.Auto.Remaining != source.Quota.Auto.Remaining {
		t.Fatalf("same token pool edit lost state: %#v", editedRecords[0])
	}

	resp = doJSON(http.MethodGet, "/admin/api/cache", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "sample.png") && !strings.Contains(resp.Body.String(), "local_image") {
		t.Fatalf("cache route failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/assets", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "asset-1") || !strings.Contains(resp.Body.String(), "token-1") {
		t.Fatalf("assets route failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/assets/delete-item", map[string]any{
		"token":    "token-1",
		"asset_id": "asset-1",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("asset delete failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/assets", nil)
	if resp.Code != http.StatusOK || strings.Contains(resp.Body.String(), "asset-1") {
		t.Fatalf("asset delete not reflected: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/assets/clear-token", map[string]any{
		"token": "token-1",
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"deleted":1`) {
		t.Fatalf("asset clear failed: %d %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(http.MethodGet, "/admin/api/assets", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"count":0`) {
		t.Fatalf("asset clear not reflected: %d %s", resp.Code, resp.Body.String())
	}

	taskResp := doJSON(http.MethodPost, "/admin/api/batch/refresh?async=true", map[string]any{
		"tokens": []string{"token-auto"},
	})
	if taskResp.Code != http.StatusOK {
		t.Fatalf("batch create failed: %d %s", taskResp.Code, taskResp.Body.String())
	}

	resp = doJSON(http.MethodPost, "/admin/api/batch/refresh", map[string]any{
		"tokens": []string{},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("batch refresh empty tokens should fail: %d %s", resp.Code, resp.Body.String())
	}
	var taskPayload map[string]any
	if err := json.Unmarshal(taskResp.Body.Bytes(), &taskPayload); err != nil {
		t.Fatalf("unmarshal task payload: %v", err)
	}
	taskID, _ := taskPayload["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task id: %s", taskResp.Body.String())
	}

	time.Sleep(100 * time.Millisecond)
	streamReq := httptest.NewRequest(http.MethodGet, "/admin/api/batch/"+taskID+"/stream", nil)
	streamReq.Header.Set("Authorization", "Bearer admin-key")
	streamResp := testutil.NewCloseNotifyRecorder()
	router.ServeHTTP(streamResp, streamReq)
	if streamResp.Code != http.StatusOK || !strings.Contains(streamResp.Body.String(), "done") {
		t.Fatalf("batch stream failed: %d %s", streamResp.Code, streamResp.Body.String())
	}
}
