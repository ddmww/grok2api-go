package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/model"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/auth"
	platformbatch "github.com/ddmww/grok2api-go/internal/platform/batch"
	"github.com/ddmww/grok2api-go/internal/platform/logging"
	"github.com/ddmww/grok2api-go/internal/platform/logstream"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
	"github.com/ddmww/grok2api-go/internal/platform/updatecheck"
	"github.com/ddmww/grok2api-go/internal/platform/upstreamblocker"
	"github.com/ddmww/grok2api-go/internal/platform/version"
	"github.com/gin-gonic/gin"
)

type updateService interface {
	GetLatestReleaseInfo(context.Context, bool) updatecheck.ReleaseInfo
}

var newUpdateService = func() updateService {
	return updatecheck.NewService(version.CleanVersion(), version.CleanCommit(), version.CleanImageTag(), "dmwdmw", "grok2api-go")
}

var (
	cfgCharReplacements = strings.NewReplacer(
		"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2212", "-",
		"\u2018", "'", "\u2019", "'", "\u201c", "\"", "\u201d", "\"",
		"\u00a0", " ", "\u2007", " ", "\u202f", " ", "\u200b", "", "\u200c", "", "\u200d", "", "\ufeff", "",
	)
	whitespaceRE        = regexp.MustCompile(`\s+`)
	startupOnlyPrefixes = []string{
		"account.storage",
		"account.local",
		"account.redis",
		"account.mysql",
		"account.postgresql",
	}
)

const tokenImportBatchSize = 1000

func Mount(router *gin.Engine, state *app.State) {
	updater := newUpdateService()
	if state.Logs == nil {
		state.Logs = logstream.NewStore(logstream.DefaultCapacity)
	}
	router.StaticFS("/static", http.Dir(paths.StaticDir()))
	router.StaticFile("/favicon.ico", filepath.Join(paths.StaticDir(), "favicon.ico"))

	router.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin") })
	router.GET("/meta", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version":    version.CleanVersion(),
			"commit":     version.CleanCommit(),
			"image_tag":  version.CleanImageTag(),
			"build_time": version.BuildTime,
		})
	})
	router.GET("/meta/update", func(c *gin.Context) {
		c.JSON(http.StatusOK, updater.GetLatestReleaseInfo(c.Request.Context(), c.Query("force") == "true"))
	})

	router.GET("/admin", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin/login") })
	router.GET("/admin/login", serveHTML("admin/login.html"))
	router.GET("/admin/account", serveHTML("admin/account.html"))
	router.GET("/admin/config", serveHTML("admin/config.html"))
	router.GET("/admin/cache", serveHTML("admin/cache.html"))
	router.GET("/admin/logs", serveHTML("admin/logs.html"))

	api := router.Group("/admin/api")
	api.Use(auth.AdminKey(state.Config))
	{
		api.GET("/verify", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "success"}) })
		api.GET("/config", func(c *gin.Context) {
			c.JSON(http.StatusOK, normalizeAdminConfig(state.Config.Raw()))
		})
		api.POST("/config", func(c *gin.Context) {
			var patch map[string]any
			if err := c.ShouldBindJSON(&patch); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			patch = sanitizeAdminConfigPatch(state.Config.Raw(), patch)
			if err := ensureRuntimePatchAllowed(patch); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			if err := state.Config.Update(c.Request.Context(), patch); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			logging.ReloadFileLogging(
				state.Config.GetString("logging.file_level", ""),
				state.Config.GetInt("logging.max_files", 7) > 0,
			)
			state.Logs.Add(logstream.Event{
				Category:   logstream.CategorySystem,
				Level:      logstream.LevelInfo,
				Path:       "/admin/api/config",
				StatusCode: http.StatusOK,
				Message:    "configuration updated",
			})
			state.Proxy.ResetAll()
			c.JSON(http.StatusOK, gin.H{"status": "success", "message": "配置已更新"})
		})
		api.GET("/storage", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"type": state.Repo.StorageType()}) })
		api.GET("/status", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok", "size": state.Runtime.Size(), "revision": state.Runtime.Revision()})
		})
		api.GET("/logs", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"items": state.Logs.List(parseLogQuery(c))})
		})
		api.GET("/logs/stream", func(c *gin.Context) {
			streamLogEvents(c, state.Logs, parseLogQuery(c))
		})
		api.POST("/logs/clear", func(c *gin.Context) {
			state.Logs.Clear()
			c.JSON(http.StatusOK, gin.H{"status": "success"})
		})
		api.POST("/sync", func(c *gin.Context) {
			if err := state.Runtime.Sync(c.Request.Context()); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"changed": true, "revision": state.Runtime.Revision()})
		})

		api.GET("/tokens", func(c *gin.Context) {
			query := parseAccountListQuery(c)
			page, err := state.Repo.ListAccounts(c.Request.Context(), query)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			summary, err := state.Repo.SummarizeAccounts(c.Request.Context(), query)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			items := make([]map[string]any, 0, len(page.Items))
			for _, item := range page.Items {
				items = append(items, serializeToken(item))
			}
			c.JSON(http.StatusOK, gin.H{
				"status":      "success",
				"items":       items,
				"tokens":      items,
				"total":       page.Total,
				"page":        page.Page,
				"page_size":   page.PageSize,
				"total_pages": page.TotalPages,
				"revision":    page.Revision,
				"summary":     summary,
			})
		})
		api.GET("/tokens/summary", func(c *gin.Context) {
			query := parseAccountListQuery(c)
			summary, err := state.Repo.SummarizeAccounts(c.Request.Context(), query)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "success", "summary": summary})
		})
		api.GET("/models", func(c *gin.Context) {
			items, err := listAvailableModels(c.Request.Context(), state.Repo)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, items)
		})

		api.POST("/tokens", func(c *gin.Context) {
			var payload map[string][]any
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			total := 0
			for pool, items := range payload {
				upserts := parseTokenImportItems(items, pool)
				if _, err := state.Repo.ReplacePool(c.Request.Context(), pool, upserts); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
					return
				}
				total += len(upserts)
			}
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"status": "success", "count": total})
		})

		api.POST("/tokens/add", func(c *gin.Context) {
			var payload struct {
				Tokens []string `json:"tokens"`
				Pool   string   `json:"pool"`
				Tags   []string `json:"tags"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			requestedPool := strings.ToLower(strings.TrimSpace(payload.Pool))
			syncAutoDetect := requestedPool == "auto"
			cleaned := sanitizeTokens(payload.Tokens)
			if len(cleaned) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"message": "No valid tokens provided"})
				return
			}

			existing, err := activeTokenSet(c.Request.Context(), state.Repo, cleaned)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}

			newTokens := make([]string, 0, len(cleaned))
			for _, token := range cleaned {
				if _, found := existing[token]; !found {
					newTokens = append(newTokens, token)
				}
			}
			if len(newTokens) == 0 {
				c.JSON(http.StatusOK, gin.H{"status": "success", "count": 0, "skipped": len(cleaned), "synced": syncAutoDetect})
				return
			}

			initialPool := account.NormalizePool(payload.Pool)
			inserted := 0
			for _, chunk := range chunkStrings(newTokens, tokenImportBatchSize) {
				upserts := make([]account.Upsert, 0, len(chunk))
				for _, token := range chunk {
					upserts = append(upserts, account.Upsert{Token: token, Pool: initialPool, Tags: account.NormalizeTags(payload.Tags)})
				}
				result, err := state.Repo.UpsertAccounts(c.Request.Context(), upserts)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
					return
				}
				inserted += result.Upserted
			}
			if syncAutoDetect {
				refreshResult, refreshErr := state.Refresh.RefreshTokens(c.Request.Context(), newTokens)
				if refreshErr != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"message": refreshErr.Error()})
					return
				}
				refreshed := refreshResult.Refreshed
				if refreshed != len(newTokens) {
					c.JSON(http.StatusInternalServerError, gin.H{
						"message": fmt.Sprintf("auto-detect refresh incomplete: refreshed %d of %d tokens", refreshed, len(newTokens)),
					})
					return
				}
			} else {
				go state.Refresh.RefreshOnImport(context.Background(), newTokens)
			}
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"status": "success", "count": inserted, "skipped": len(cleaned) - len(newTokens), "synced": syncAutoDetect})
		})

		api.POST("/tokens/export", func(c *gin.Context) {
			var payload struct {
				Format string `json:"format"`
				batchSelectionPayload
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			format := strings.ToLower(strings.TrimSpace(payload.Format))
			if format == "" {
				format = "txt"
			}
			if format != "txt" && format != "json" {
				c.JSON(http.StatusBadRequest, gin.H{"message": "format must be txt or json"})
				return
			}

			records, err := resolveExportSelection(c.Request.Context(), state.Repo, payload.batchSelectionPayload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			if len(records) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"message": "No tokens provided"})
				return
			}

			filename := fmt.Sprintf("tokens-%s.%s", exportSuffix(payload.Filters.Pool, payload.Filters.Status, payload.Filters.NSFW), format)
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
			if format == "json" {
				grouped := map[string][]map[string]any{}
				for _, record := range records {
					pool := account.NormalizePool(record.Pool)
					grouped[pool] = append(grouped[pool], map[string]any{
						"token": record.Token,
						"tags":  record.Tags,
					})
				}
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(http.StatusOK, grouped)
				return
			}

			lines := make([]string, 0, len(records))
			for _, record := range records {
				if strings.TrimSpace(record.Token) != "" {
					lines = append(lines, record.Token)
				}
			}
			content := strings.Join(lines, "\n")
			if content != "" {
				content += "\n"
			}
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(content))
		})

		api.DELETE("/tokens", func(c *gin.Context) {
			payload, err := parseBatchSelectionPayload(c)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			tokens, err := resolveBatchSelection(c.Request.Context(), state.Repo, payload, false)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			result, err := state.Repo.DeleteAccounts(c.Request.Context(), tokens)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"deleted": result.Deleted})
		})

		api.PUT("/tokens/edit", func(c *gin.Context) {
			var payload struct {
				OldToken string `json:"old_token"`
				Token    string `json:"token"`
				Pool     string `json:"pool"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			oldToken := account.NormalizeToken(payload.OldToken)
			newToken := account.NormalizeToken(payload.Token)
			pool := account.NormalizePool(payload.Pool)
			if oldToken == "" || newToken == "" {
				c.JSON(http.StatusBadRequest, gin.H{"message": "Token is required"})
				return
			}
			records, err := state.Repo.GetAccounts(c.Request.Context(), []string{oldToken})
			if err != nil || len(records) == 0 {
				c.JSON(http.StatusNotFound, gin.H{"message": "Account not found"})
				return
			}
			record := records[0]
			if oldToken != newToken {
				targetRecords, err := state.Repo.GetAccounts(c.Request.Context(), []string{newToken})
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
					return
				}
				if len(targetRecords) > 0 {
					c.JSON(http.StatusConflict, gin.H{"message": "Target token already exists"})
					return
				}
			}
			if oldToken == newToken {
				_, err := state.Repo.PatchAccounts(c.Request.Context(), []account.Patch{{
					Token: oldToken,
					Pool:  ptrString(pool),
				}})
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
					return
				}
				_ = state.Runtime.Sync(c.Request.Context())
				c.JSON(http.StatusOK, gin.H{"status": "success", "token": newToken, "pool": pool})
				return
			}
			upsert := account.Upsert{Token: newToken, Pool: pool, Tags: record.Tags, Ext: record.Ext}
			if _, err := state.Repo.UpsertAccounts(c.Request.Context(), []account.Upsert{upsert}); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			patch := account.Patch{
				Token:          newToken,
				Pool:           ptrString(pool),
				Status:         ptrStatus(record.Status),
				Tags:           record.Tags,
				Quota:          quotaPatch(record),
				UsageUseDelta:  ptrInt(record.UsageUseCount),
				UsageFailDelta: ptrInt(record.UsageFailCount),
				UsageSyncDelta: ptrInt(record.UsageSyncCount),
				LastUseAt:      ptrInt64(record.LastUseAt),
				LastFailAt:     ptrInt64(record.LastFailAt),
				LastFailReason: ptrString(record.LastFailReason),
				LastSyncAt:     ptrInt64(record.LastSyncAt),
				LastClearAt:    ptrInt64(record.LastClearAt),
				StateReason:    ptrString(record.StateReason),
				ExtMerge:       record.Ext,
			}
			if _, err := state.Repo.PatchAccounts(c.Request.Context(), []account.Patch{patch}); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			_, _ = state.Repo.DeleteAccounts(c.Request.Context(), []string{oldToken})
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"status": "success", "token": newToken, "pool": pool})
		})

		api.POST("/tokens/disabled", func(c *gin.Context) {
			var payload struct {
				Token    string `json:"token"`
				Disabled bool   `json:"disabled"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			status := account.StatusActive
			if payload.Disabled {
				status = account.StatusDisabled
			}
			_, err := state.Repo.PatchAccounts(c.Request.Context(), []account.Patch{{
				Token:         payload.Token,
				Status:        ptrStatus(status),
				StateReason:   ptrString("operator_disabled"),
				ClearFailures: !payload.Disabled,
			}})
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"status": "success", "token": payload.Token, "disabled": payload.Disabled})
		})

		api.PUT("/tokens/pool", func(c *gin.Context) {
			var payload struct {
				Pool   string   `json:"pool"`
				Tokens []string `json:"tokens"`
				Tags   []string `json:"tags"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			upserts := make([]account.Upsert, 0, len(payload.Tokens))
			for _, token := range payload.Tokens {
				upserts = append(upserts, account.Upsert{Token: token, Pool: payload.Pool, Tags: payload.Tags})
			}
			result, err := state.Repo.ReplacePool(c.Request.Context(), payload.Pool, upserts)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"pool": payload.Pool, "count": result.Upserted})
		})

		api.GET("/cache", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"local_image": dirStats(paths.ImageCacheDir()),
				"local_video": dirStats(paths.VideoCacheDir()),
			})
		})
		api.GET("/cache/list", func(c *gin.Context) {
			kind := c.DefaultQuery("type", "image")
			page := 1
			pageSize := 1000
			if value := c.Query("page"); value != "" {
				_, _ = fmt.Sscanf(value, "%d", &page)
			}
			if value := c.Query("page_size"); value != "" {
				_, _ = fmt.Sscanf(value, "%d", &pageSize)
			}
			c.JSON(http.StatusOK, listLocal(kind, page, pageSize))
		})
		api.POST("/cache/clear", func(c *gin.Context) {
			var payload struct {
				Type string `json:"type"`
			}
			_ = c.ShouldBindJSON(&payload)
			removed := clearLocal(payload.Type)
			c.JSON(http.StatusOK, gin.H{"status": "success", "result": gin.H{"removed": removed}})
		})
		api.POST("/cache/item/delete", func(c *gin.Context) {
			var payload struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			if err := os.Remove(filepath.Join(cacheDir(payload.Type), payload.Name)); err != nil && !os.IsNotExist(err) {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "success", "result": gin.H{"deleted": payload.Name}})
		})
		api.POST("/cache/items/delete", func(c *gin.Context) {
			var payload struct {
				Type  string   `json:"type"`
				Names []string `json:"names"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			deleted := 0
			for _, name := range payload.Names {
				if err := os.Remove(filepath.Join(cacheDir(payload.Type), name)); err == nil {
					deleted++
				}
			}
			c.JSON(http.StatusOK, gin.H{"status": "success", "result": gin.H{"deleted": deleted, "missing": len(payload.Names) - deleted}})
		})

		api.GET("/assets", func(c *gin.Context) {
			rows, total, err := listAllAssets(c.Request.Context(), state)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"tokens": rows, "total_assets": total})
		})
		api.POST("/assets/delete-item", func(c *gin.Context) {
			var payload struct {
				Token   string `json:"token"`
				AssetID string `json:"asset_id"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			token := strings.TrimSpace(payload.Token)
			assetID := strings.TrimSpace(payload.AssetID)
			if token == "" || assetID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"message": "token and asset_id are required"})
				return
			}
			if err := state.XAI.DeleteAsset(c.Request.Context(), token, assetID); err != nil {
				applyAssetErrorFeedback(c.Request.Context(), state, token, err)
				c.JSON(httpStatusForAssetError(err), gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "success"})
		})
		api.POST("/assets/clear-token", func(c *gin.Context) {
			var payload struct {
				Token string `json:"token"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			token := strings.TrimSpace(payload.Token)
			if token == "" {
				c.JSON(http.StatusBadRequest, gin.H{"message": "token is required"})
				return
			}
			items, err := state.XAI.ListAssets(c.Request.Context(), token)
			if err != nil {
				applyAssetErrorFeedback(c.Request.Context(), state, token, err)
				c.JSON(httpStatusForAssetError(err), gin.H{"message": err.Error()})
				return
			}
			deleted := 0
			for _, item := range items {
				assetID := assetIdentifier(item)
				if assetID == "" {
					continue
				}
				if err := state.XAI.DeleteAsset(c.Request.Context(), token, assetID); err != nil {
					applyAssetErrorFeedback(c.Request.Context(), state, token, err)
					c.JSON(httpStatusForAssetError(err), gin.H{"message": err.Error()})
					return
				}
				deleted++
			}
			c.JSON(http.StatusOK, gin.H{"status": "success", "deleted": deleted})
		})

		api.POST("/batch/refresh", func(c *gin.Context) { dispatchBatch(c, state, "refresh", true) })
		api.POST("/batch/nsfw", func(c *gin.Context) {
			enabled := true
			if strings.EqualFold(c.Query("enabled"), "false") {
				enabled = false
			}
			dispatchBatch(c, state, "nsfw", enabled)
		})
		api.POST("/batch/cache-clear", func(c *gin.Context) { dispatchBatch(c, state, "cache-clear", true) })
		api.GET("/batch/:task_id/stream", func(c *gin.Context) { streamTask(c, state.Tasks) })
		api.POST("/batch/:task_id/cancel", func(c *gin.Context) {
			task := state.Tasks.Get(c.Param("task_id"))
			if task == nil {
				c.JSON(http.StatusNotFound, gin.H{"message": "Task not found"})
				return
			}
			task.Cancel()
			c.JSON(http.StatusOK, gin.H{"status": "success"})
		})
	}
}

func serveHTML(path string) gin.HandlerFunc {
	return func(c *gin.Context) {
		filePath := filepath.Join(paths.StaticDir(), path)
		body, err := os.ReadFile(filePath)
		if err != nil {
			c.String(http.StatusNotFound, "not found")
			return
		}
		rendered := strings.ReplaceAll(string(body), "{{APP_VERSION}}", version.Current)
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(rendered))
	}
}

func parseLogQuery(c *gin.Context) logstream.Query {
	limit := 200
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			limit = value
		}
	}
	return logstream.Query{
		Category: logstream.NormalizeCategory(c.Query("category")),
		Level:    logstream.NormalizeLevel(c.Query("level")),
		Limit:    limit,
	}
}

func streamLogEvents(c *gin.Context, store *logstream.Store, query logstream.Query) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	events, cancel := store.Subscribe(query)
	defer cancel()
	_, _ = c.Writer.Write([]byte(": connected\n\n"))
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := c.Writer.Write([]byte("event: log\n" + "data: " + string(data) + "\n\n")); err != nil {
				return
			}
			if flusher, ok := c.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

func serializeToken(item account.Record) map[string]any {
	quota := map[string]any{}
	for mode, value := range item.Quota.ToMap() {
		quota[mode] = map[string]any{
			"remaining": value.Remaining,
			"total":     value.Total,
		}
	}
	return map[string]any{
		"token":        item.Token,
		"pool":         item.Pool,
		"status":       item.Status,
		"quota":        quota,
		"use_count":    item.UsageUseCount,
		"fail_count":   item.UsageFailCount,
		"last_used_at": item.LastUseAt,
		"tags":         item.Tags,
	}
}

func sanitizeTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	seen := map[string]struct{}{}
	for _, raw := range tokens {
		token := account.NormalizeToken(raw)
		if token == "" {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func chunkStrings(items []string, size int) [][]string {
	if len(items) == 0 {
		return nil
	}
	if size <= 0 {
		size = tokenImportBatchSize
	}
	chunks := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[start:end])
	}
	return chunks
}

func activeTokenSet(ctx context.Context, repo account.Repository, tokens []string) (map[string]struct{}, error) {
	existing := map[string]struct{}{}
	for _, chunk := range chunkStrings(tokens, tokenImportBatchSize) {
		records, err := repo.GetAccounts(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if !record.IsDeleted() {
				existing[record.Token] = struct{}{}
			}
		}
	}
	return existing, nil
}

func parseTokenImportItems(items []any, pool string) []account.Upsert {
	normalizedPool := account.NormalizePool(pool)
	out := make([]account.Upsert, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		token := ""
		tags := []string(nil)
		switch typed := item.(type) {
		case string:
			token = typed
		case map[string]any:
			if raw, ok := typed["token"].(string); ok {
				token = raw
			}
			tags = normalizeAnyStringSlice(typed["tags"])
		}
		token = account.NormalizeToken(token)
		if token == "" {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, account.Upsert{
			Token: token,
			Pool:  normalizedPool,
			Tags:  account.NormalizeTags(tags),
		})
	}
	return out
}

func quotaPatch(record account.Record) map[string]account.QuotaWindow {
	out := map[string]account.QuotaWindow{
		"auto":   record.Quota.Auto,
		"fast":   record.Quota.Fast,
		"expert": record.Quota.Expert,
	}
	if record.Quota.Heavy != nil {
		out["heavy"] = record.Quota.Heavy.Clone()
	}
	if record.Quota.Grok4_3 != nil {
		out["grok_4_3"] = record.Quota.Grok4_3.Clone()
	}
	return out
}

func listAvailableModels(_ context.Context, _ account.Repository) (gin.H, error) {
	grouped := map[string][]gin.H{
		"chat":       {},
		"image":      {},
		"image_edit": {},
		"video":      {},
	}
	for _, spec := range model.All() {
		grouped[modelCategory(spec)] = append(grouped[modelCategory(spec)], gin.H{
			"id":          spec.Name,
			"name":        spec.Name,
			"public_name": spec.PublicName,
			"pool":        spec.Pool,
			"mode":        spec.Mode,
		})
	}

	return gin.H{
		"groups": grouped,
		"mode":   "static",
	}, nil
}

func modelCategory(spec model.Spec) string {
	switch {
	case spec.IsImageEdit():
		return "image_edit"
	case spec.IsImage():
		return "image"
	case spec.IsVideo():
		return "video"
	default:
		return "chat"
	}
}

func slicesContains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func listAllAssets(ctx context.Context, state *app.State) ([]map[string]any, int, error) {
	tokens, err := listManageableTokens(ctx, state.Repo)
	if err != nil {
		return nil, 0, err
	}
	if len(tokens) == 0 {
		return []map[string]any{}, 0, nil
	}

	rows := make([]map[string]any, len(tokens))
	var total int
	var totalMu sync.Mutex
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 8)

	for index, token := range tokens {
		wg.Add(1)
		go func(idx int, token string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			items, err := state.XAI.ListAssets(ctx, token)
			if err != nil {
				applyAssetErrorFeedback(ctx, state, token, err)
				rows[idx] = assetRow(token, nil, err.Error())
				return
			}
			rows[idx] = assetRow(token, items, "")
			totalMu.Lock()
			total += len(items)
			totalMu.Unlock()
		}(index, token)
	}
	wg.Wait()
	return rows, total, nil
}

func listManageableTokens(ctx context.Context, repo account.Repository) ([]string, error) {
	tokens, err := listTokensByQuery(ctx, repo, account.ListQuery{
		Page:           1,
		PageSize:       2000,
		IncludeDeleted: false,
		SortBy:         "created_at",
		SortDesc:       true,
	}, true)
	if err != nil {
		return nil, err
	}
	sort.Strings(tokens)
	return tokens, nil
}

func assetRow(token string, items []map[string]any, errorText string) map[string]any {
	assets := make([]map[string]any, 0, len(items))
	for _, item := range items {
		assets = append(assets, map[string]any{
			"id":           stringValue(item["id"], item["assetId"]),
			"name":         stringValue(item["fileName"], item["name"]),
			"file_path":    stringValue(item["filePath"], item["file_path"]),
			"content_type": stringValue(item["contentType"], item["content_type"]),
			"size":         intValue(item["fileSize"], item["size"]),
			"created_at":   stringValue(item["createdAt"], item["created_at"]),
		})
	}
	return map[string]any{
		"token":  token,
		"masked": maskToken(token),
		"count":  len(assets),
		"assets": assets,
		"error":  errorText,
	}
}

func assetIdentifier(item map[string]any) string {
	return stringValue(item["id"], item["assetId"])
}

func maskToken(token string) string {
	if len(token) <= 20 {
		return token
	}
	return token[:8] + "..." + token[len(token)-8:]
}

func stringValue(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return typed
			}
		}
	}
	return ""
}

func intValue(values ...any) int64 {
	for _, value := range values {
		switch typed := value.(type) {
		case int:
			return int64(typed)
		case int64:
			return typed
		case float64:
			return int64(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func httpStatusForAssetError(err error) int {
	var upstreamErr *xai.UpstreamError
	if errors.As(err, &upstreamErr) && upstreamErr.Status > 0 {
		return upstreamErr.Status
	}
	return http.StatusBadGateway
}

func applyAssetErrorFeedback(ctx context.Context, state *app.State, token string, err error) {
	var upstreamErr *xai.UpstreamError
	if !errors.As(err, &upstreamErr) {
		return
	}
	lease := &account.Lease{Token: token}
	switch upstreamErr.Status {
	case http.StatusUnauthorized:
		_ = state.Runtime.ApplyFeedback(ctx, lease, account.Feedback{Kind: account.FeedbackUnauthorized, Reason: err.Error()})
	case http.StatusForbidden:
		_ = state.Runtime.ApplyFeedback(ctx, lease, account.Feedback{Kind: account.FeedbackForbidden, Reason: err.Error()})
	case http.StatusTooManyRequests:
		_ = state.Runtime.ApplyFeedback(ctx, lease, account.Feedback{Kind: account.FeedbackRateLimited, Reason: err.Error()})
	}
}

func normalizeAnyStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := []string{}
		for _, item := range typed {
			out = append(out, strings.TrimSpace(fmt.Sprint(item)))
		}
		return out
	default:
		return nil
	}
}

func dispatchBatch(c *gin.Context, state *app.State, kind string, enabled bool) {
	payload, err := parseBatchSelectionPayload(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
		return
	}
	tokens, err := resolveBatchSelection(c.Request.Context(), state.Repo, payload, kind != "refresh")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
		return
	}
	if len(tokens) == 0 {
		switch kind {
		case "refresh":
			c.JSON(http.StatusBadRequest, gin.H{"message": "No tokens provided"})
			return
		case "nsfw", "cache-clear":
			if len(tokens) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"message": "No tokens available"})
				return
			}
		}
	}
	asyncMode := c.Query("async") == "true"
	concurrency := batchConcurrency(c, state, kind)
	if asyncMode {
		task := state.Tasks.Create(len(tokens))
		go runBatchTask(task, state, kind, enabled, tokens, concurrency)
		c.JSON(http.StatusOK, gin.H{"status": "success", "task_id": task.ID, "total": len(tokens)})
		return
	}
	result := runBatch(context.Background(), state, kind, enabled, tokens, concurrency)
	c.JSON(http.StatusOK, gin.H{"status": "success", "summary": gin.H{"total": len(tokens), "ok": result.ok, "fail": result.fail}, "results": result.items})
}

type batchSelectionPayload struct {
	Tokens            []string `json:"tokens"`
	SelectAllFiltered bool     `json:"select_all_filtered"`
	Filters           struct {
		Pool   string `json:"pool"`
		Status string `json:"status"`
		NSFW   string `json:"nsfw"`
	} `json:"filters"`
}

func parseBatchSelectionPayload(c *gin.Context) (batchSelectionPayload, error) {
	var payload batchSelectionPayload
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return payload, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return payload, nil
	}
	var asTokens []string
	if err := json.Unmarshal(body, &asTokens); err == nil {
		payload.Tokens = asTokens
		return payload, nil
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func resolveBatchSelection(ctx context.Context, repo account.Repository, payload batchSelectionPayload, fallbackManageable bool) ([]string, error) {
	tokens := sanitizeTokens(payload.Tokens)
	if payload.SelectAllFiltered {
		return listTokensByQuery(ctx, repo, account.ListQuery{
			Page:           1,
			PageSize:       2000,
			Pool:           normalizeBatchFilterValue(payload.Filters.Pool),
			Status:         account.Status(normalizeBatchFilterValue(payload.Filters.Status)),
			NSFW:           normalizeBatchFilterValue(payload.Filters.NSFW),
			IncludeDeleted: false,
			SortBy:         "created_at",
			SortDesc:       true,
		}, false)
	}
	if len(tokens) > 0 {
		return tokens, nil
	}
	if fallbackManageable {
		return listManageableTokens(ctx, repo)
	}
	return nil, nil
}

func resolveExportSelection(ctx context.Context, repo account.Repository, payload batchSelectionPayload) ([]account.Record, error) {
	if payload.SelectAllFiltered {
		return listRecordsByQuery(ctx, repo, account.ListQuery{
			Page:           1,
			PageSize:       1000,
			Pool:           normalizeBatchFilterValue(payload.Filters.Pool),
			Status:         account.Status(normalizeBatchFilterValue(payload.Filters.Status)),
			NSFW:           normalizeBatchFilterValue(payload.Filters.NSFW),
			IncludeDeleted: false,
			SortBy:         "created_at",
			SortDesc:       true,
		})
	}

	tokens := sanitizeTokens(payload.Tokens)
	if len(tokens) == 0 {
		return nil, nil
	}
	records, err := repo.GetAccounts(ctx, tokens)
	if err != nil {
		return nil, err
	}
	byToken := make(map[string]account.Record, len(records))
	for _, record := range records {
		if !record.IsDeleted() {
			byToken[record.Token] = record
		}
	}
	ordered := make([]account.Record, 0, len(tokens))
	for _, token := range tokens {
		if record, ok := byToken[token]; ok {
			ordered = append(ordered, record)
		}
	}
	return ordered, nil
}

func listRecordsByQuery(ctx context.Context, repo account.Repository, query account.ListQuery) ([]account.Record, error) {
	pageNum := 1
	records := make([]account.Record, 0, 128)
	for {
		query.Page = pageNum
		page, err := repo.ListAccounts(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Items {
			if !record.IsDeleted() {
				records = append(records, record)
			}
		}
		if int64(pageNum*query.PageSize) >= page.Total || len(page.Items) == 0 {
			break
		}
		pageNum++
	}
	return records, nil
}

func normalizeBatchFilterValue(value string) string {
	normalized := strings.TrimSpace(value)
	if strings.EqualFold(normalized, "all") {
		return ""
	}
	return normalized
}

func exportSuffix(pool, status, nsfw string) string {
	parts := []string{
		defaultIfEmpty(normalizeBatchFilterValue(status), "all"),
		defaultIfEmpty(normalizeBatchFilterValue(nsfw), "all"),
		defaultIfEmpty(normalizeBatchFilterValue(pool), "all"),
	}
	return strings.Join(parts, "-")
}

func defaultIfEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func listTokensByQuery(ctx context.Context, repo account.Repository, query account.ListQuery, onlyManageable bool) ([]string, error) {
	pageNum := 1
	tokens := make([]string, 0, 64)
	now := account.NowMS()
	for {
		query.Page = pageNum
		page, err := repo.ListAccounts(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Items {
			if record.IsDeleted() {
				continue
			}
			if onlyManageable {
				status := record.EffectiveStatus(now)
				if status != account.StatusActive && status != account.StatusCooling {
					continue
				}
			}
			tokens = append(tokens, record.Token)
		}
		if int64(pageNum*query.PageSize) >= page.Total || len(page.Items) == 0 {
			break
		}
		pageNum++
	}
	return tokens, nil
}

type batchResult struct {
	ok    int
	fail  int
	items map[string]any
}

func runBatchTask(task *tasks.Task, state *app.State, kind string, enabled bool, tokens []string, concurrency int) {
	result := runBatchWithRecorder(context.Background(), state, kind, enabled, tokens, concurrency, func(token string, item map[string]any, err error) {
		if task.Cancelled {
			return
		}
		if err != nil {
			task.Record(false, token, nil, err.Error())
			return
		}
		task.Record(true, token, item, "")
	})
	if task.Cancelled {
		task.FinishCancelled()
		return
	}
	task.Finish(map[string]any{
		"status":  "success",
		"summary": map[string]any{"total": len(tokens), "ok": result.ok, "fail": result.fail},
		"results": result.items,
	}, "")
}

func runBatch(ctx context.Context, state *app.State, kind string, enabled bool, tokens []string, concurrency int) batchResult {
	return runBatchWithRecorder(ctx, state, kind, enabled, tokens, concurrency, nil)
}

func runBatchWithRecorder(ctx context.Context, state *app.State, kind string, enabled bool, tokens []string, concurrency int, onItem func(string, map[string]any, error)) batchResult {
	type batchItemResult struct {
		token string
		item  map[string]any
		patch *account.Patch
		err   error
	}
	results := platformbatch.Run(tokens, concurrency, func(token string) batchItemResult {
		item, patch, err := executeBatchItem(ctx, state, kind, enabled, token)
		return batchItemResult{token: token, item: item, patch: patch, err: err}
	})
	patches := make([]account.Patch, 0, len(results))
	out := batchResult{items: map[string]any{}}
	for _, result := range results {
		if result.patch != nil && result.err == nil {
			patches = append(patches, *result.patch)
		}
	}
	if len(patches) > 0 {
		if _, err := state.Repo.PatchAccounts(ctx, patches); err != nil {
			for index := range results {
				if results[index].patch != nil && results[index].err == nil {
					results[index].err = err
					results[index].item = nil
				}
			}
		} else {
			_ = state.Runtime.Sync(ctx)
		}
	}
	for _, result := range results {
		if result.err != nil {
			out.fail++
			out.items[result.token] = gin.H{"error": result.err.Error()}
		} else {
			out.ok++
			out.items[result.token] = result.item
		}
		if onItem != nil {
			onItem(result.token, result.item, result.err)
		}
	}
	return out
}

func executeBatchItem(ctx context.Context, state *app.State, kind string, enabled bool, token string) (map[string]any, *account.Patch, error) {
	switch kind {
	case "refresh":
		result, err := state.Refresh.RefreshTokens(ctx, []string{token})
		if err != nil {
			return nil, nil, err
		}
		if result.Refreshed == 0 {
			return nil, nil, errors.New("未获取到真实配额数据")
		}
		return map[string]any{"refreshed": result.Refreshed}, nil, nil
	case "nsfw":
		session, err := state.XAI.NewRequestSession(false)
		if err != nil {
			return nil, nil, err
		}
		defer session.Close()
		if enabled {
			if err := session.AcceptTOS(ctx, token); err != nil {
				return nil, nil, err
			}
			if err := session.SetBirthDate(ctx, token); err != nil {
				return nil, nil, err
			}
		}
		if err := session.SetNSFW(ctx, token, enabled); err != nil {
			return nil, nil, err
		}
		addTags := []string{"nsfw"}
		removeTags := []string{}
		if !enabled {
			addTags, removeTags = nil, []string{"nsfw"}
		}
		patch := &account.Patch{Token: token, AddTags: addTags, RemoveTags: removeTags}
		return map[string]any{"success": true, "tagged": enabled}, patch, nil
	case "cache-clear":
		session, err := state.XAI.NewRequestSession(true)
		if err != nil {
			return nil, nil, err
		}
		defer session.Close()
		items, err := session.ListAssets(ctx, token)
		if err != nil {
			return nil, nil, err
		}
		deleted := 0
		for _, item := range items {
			assetID, _ := item["id"].(string)
			if assetID == "" {
				assetID, _ = item["assetId"].(string)
			}
			if assetID == "" {
				continue
			}
			if err := session.DeleteAsset(ctx, token, assetID); err == nil {
				deleted++
			}
		}
		return map[string]any{"deleted": deleted}, nil, nil
	default:
		return nil, nil, errors.New("unsupported batch operation")
	}
}

func streamTask(c *gin.Context, store *tasks.Store) {
	task := store.Get(c.Param("task_id"))
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"message": "Task not found"})
		return
	}
	queue := task.Attach()
	defer task.Detach(queue)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	writeEvent := func(w io.Writer, event any) bool {
		data, err := json.Marshal(event)
		if err != nil {
			return false
		}
		_, err = w.Write([]byte("data: " + string(data) + "\n\n"))
		return err == nil
	}

	c.Stream(func(w io.Writer) bool {
		if snapshot := task.Snapshot(); snapshot != nil {
			snapshot["type"] = "snapshot"
			if !writeEvent(w, snapshot) {
				return false
			}
		}
		if final := task.Final(); final != nil {
			_ = writeEvent(w, final)
			return false
		}
		for {
			select {
			case <-c.Request.Context().Done():
				return false
			case event, ok := <-queue:
				if !ok {
					return false
				}
				if !writeEvent(w, event) {
					return false
				}
				switch event["type"] {
				case "done", "error", "cancelled":
					return false
				}
			case <-time.After(15 * time.Second):
				_, _ = w.Write([]byte(": ping\n\n"))
				return true
			}
		}
	})
}

func cacheDir(kind string) string {
	if kind == "video" {
		return paths.VideoCacheDir()
	}
	return paths.ImageCacheDir()
}

func dirStats(dir string) gin.H {
	items, _ := os.ReadDir(dir)
	count := 0
	var total int64
	for _, item := range items {
		if !item.Type().IsRegular() {
			continue
		}
		count++
		if info, err := item.Info(); err == nil {
			total += info.Size()
		}
	}
	return gin.H{"count": count, "size_mb": float64(total) / 1024 / 1024}
}

func listLocal(kind string, page, pageSize int) gin.H {
	dir := cacheDir(kind)
	items, _ := os.ReadDir(dir)
	records := make([]gin.H, 0, len(items))
	for _, item := range items {
		if !item.Type().IsRegular() {
			continue
		}
		info, _ := item.Info()
		records = append(records, gin.H{"name": item.Name(), "size_bytes": info.Size(), "modified_at": info.ModTime().Unix()})
	}
	sort.SliceStable(records, func(i, j int) bool {
		return intValue(records[i]["modified_at"]) > intValue(records[j]["modified_at"])
	})
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > len(records) {
		end = len(records)
	}
	if start > end {
		start = end
	}
	return gin.H{"total": len(records), "page": page, "page_size": pageSize, "items": records[start:end], "status": "success"}
}

func clearLocal(kind string) int {
	dir := cacheDir(kind)
	items, _ := os.ReadDir(dir)
	removed := 0
	for _, item := range items {
		if item.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, item.Name())); err == nil {
				removed++
			}
		}
	}
	return removed
}

func ptrInt(value int) *int                          { return &value }
func ptrInt64(value int64) *int64                    { return &value }
func ptrString(value string) *string                 { return &value }
func ptrStatus(value account.Status) *account.Status { return &value }

func sanitizeText(value any, removeAllSpaces bool) string {
	text := strings.TrimSpace(fmt.Sprint(value))
	text = cfgCharReplacements.Replace(text)
	if removeAllSpaces {
		text = whitespaceRE.ReplaceAllString(text, "")
	} else {
		text = strings.TrimSpace(text)
	}
	return text
}

func sanitizeProxyConfig(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	if _, ok := payload["proxy"].(map[string]any); !ok {
		return payload
	}
	sanitizeFields := func(section map[string]any) {
		for _, field := range []struct {
			key             string
			removeAllSpaces bool
		}{
			{key: "user_agent"},
			{key: "cf_cookies"},
			{key: "cf_clearance", removeAllSpaces: true},
		} {
			value, exists := section[field.key]
			if !exists {
				continue
			}
			section[field.key] = sanitizeText(value, field.removeAllSpaces)
		}
	}
	next := cloneMapAny(payload)
	proxyCopy, _ := next["proxy"].(map[string]any)
	sanitizeFields(proxyCopy)
	if clearance, ok := proxyCopy["clearance"].(map[string]any); ok {
		sanitizeFields(clearance)
		proxyCopy["clearance"] = clearance
	}
	next["proxy"] = proxyCopy
	return next
}

func normalizeAdminConfig(payload map[string]any) map[string]any {
	next := sanitizeProxyConfig(payload)
	return syncUpstreamBlockerConfigPatch(nil, next)
}

func sanitizeAdminConfigPatch(current map[string]any, payload map[string]any) map[string]any {
	next := sanitizeProxyConfig(payload)
	return syncUpstreamBlockerConfigPatch(current, next)
}

func syncUpstreamBlockerConfigPatch(current map[string]any, payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	next := cloneMapAny(payload)
	currentBlocker := map[string]any{}
	if rawCurrent, ok := current["upstream_blocker"].(map[string]any); ok {
		currentBlocker = cloneMapAny(rawCurrent)
	}

	rawBlocker, exists := next["upstream_blocker"]
	if !exists {
		if len(currentBlocker) == 0 {
			return next
		}
		rawBlocker = currentBlocker
		exists = true
	}

	blockerPatch, ok := rawBlocker.(map[string]any)
	if !ok {
		return next
	}
	blocker := currentBlocker
	if blocker == nil {
		blocker = map[string]any{}
	}
	mergeMapAny(blocker, blockerPatch)

	keywords := upstreamblocker.NormalizeKeywords(blocker["keywords"])
	message := normalizeUpstreamBlockerMessage(blocker["message"])
	if message == "" || message == "<nil>" {
		message = upstreamblocker.DefaultMessage
	}
	normalized := map[string]any{
		"enabled":        normalizeBoolValue(blocker["enabled"]),
		"case_sensitive": normalizeBoolValue(blocker["case_sensitive"]),
		"keywords":       keywords,
		"message":        message,
	}
	if normalized["enabled"].(bool) && len(keywords) == 0 {
		normalized["__validation_error"] = "启用上游拦截时，至少需要配置一个关键词。"
	}
	if normalized["enabled"].(bool) && blockerPatch != nil {
		if rawMessage, exists := blockerPatch["message"]; exists && normalizeUpstreamBlockerMessage(rawMessage) == "" {
			normalized["__validation_error"] = "上游拦截提示文案不能为空。"
		}
	}
	next["upstream_blocker"] = normalized
	return next
}

func normalizeUpstreamBlockerMessage(value any) string {
	if value == nil {
		return ""
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return ""
	}
	return text
}

func normalizeBoolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
	case int:
		return typed != 0
	case int32:
		return typed != 0
	case int64:
		return typed != 0
	case float32:
		return typed != 0
	case float64:
		return typed != 0
	}
	return false
}

func mergeMapAny(dst map[string]any, src map[string]any) {
	for key, value := range src {
		child, ok := value.(map[string]any)
		if !ok {
			dst[key] = value
			continue
		}
		existing, ok := dst[key].(map[string]any)
		if !ok {
			dst[key] = cloneMapAny(child)
			continue
		}
		mergeMapAny(existing, child)
	}
}

func ensureRuntimePatchAllowed(payload map[string]any) error {
	if blocker, ok := payload["upstream_blocker"].(map[string]any); ok {
		if raw, exists := blocker["__validation_error"]; exists {
			return fmt.Errorf("%v", raw)
		}
	}
	for _, path := range patchPaths(payload, "") {
		for _, blocked := range startupOnlyPrefixes {
			if path == blocked || strings.HasPrefix(path, blocked+".") {
				return fmt.Errorf("Storage config is startup-only and must be set via env")
			}
		}
	}
	return nil
}

func patchPaths(value any, prefix string) []string {
	node, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := []string{}
	for key, child := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if _, ok := child.(map[string]any); ok {
			out = append(out, patchPaths(child, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}

func cloneMapAny(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = cloneMapAny(typed)
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

func batchConcurrency(c *gin.Context, state *app.State, kind string) int {
	if raw := strings.TrimSpace(c.Query("concurrency")); raw != "" {
		var parsed int
		if _, err := fmt.Sscanf(raw, "%d", &parsed); err == nil && parsed > 0 {
			return parsed
		}
	}
	key := "batch.refresh_concurrency"
	switch kind {
	case "nsfw":
		key = "batch.nsfw_concurrency"
	case "cache-clear":
		key = "batch.asset_delete_concurrency"
	}
	value := state.Config.GetInt(key, 10)
	if value <= 0 {
		return 10
	}
	return value
}

func parseAccountListQuery(c *gin.Context) account.ListQuery {
	page := 1
	pageSize := 50
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		_, _ = fmt.Sscanf(raw, "%d", &page)
	}
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		_, _ = fmt.Sscanf(raw, "%d", &pageSize)
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 2000 {
		pageSize = 2000
	}
	return account.ListQuery{
		Page:           page,
		PageSize:       pageSize,
		Pool:           strings.TrimSpace(c.Query("pool")),
		Status:         account.Status(strings.TrimSpace(c.Query("status"))),
		NSFW:           strings.TrimSpace(c.Query("nsfw")),
		IncludeDeleted: false,
		SortBy:         "created_at",
		SortDesc:       true,
	}
}
