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
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/platform/auth"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
	"github.com/ddmww/grok2api-go/internal/platform/version"
	"github.com/gin-gonic/gin"
)

func Mount(router *gin.Engine, state *app.State) {
	router.StaticFS("/static", http.Dir(paths.StaticDir()))
	router.StaticFile("/favicon.ico", filepath.Join(paths.StaticDir(), "favicon.ico"))

	router.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin") })
	router.GET("/meta", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"version": version.Current}) })
	router.GET("/meta/update", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"current_version": version.Current,
			"latest_version":  version.Current,
			"has_update":      false,
			"release_notes":   "",
		})
	})

	router.GET("/admin", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin/login") })
	router.GET("/admin/login", serveHTML("admin/login.html"))
	router.GET("/admin/account", serveHTML("admin/account.html"))
	router.GET("/admin/config", serveHTML("admin/config.html"))
	router.GET("/admin/cache", serveHTML("admin/cache.html"))

	api := router.Group("/admin/api")
	api.Use(auth.AdminKey(state.Config))
	{
		api.GET("/verify", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "success"}) })
		api.GET("/config", func(c *gin.Context) { c.JSON(http.StatusOK, state.Config.Raw()) })
		api.POST("/config", func(c *gin.Context) {
			var patch map[string]any
			if err := c.ShouldBindJSON(&patch); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			if err := state.Config.Update(c.Request.Context(), patch); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			state.Proxy.ResetAll()
			c.JSON(http.StatusOK, gin.H{"status": "success", "message": "配置已更新"})
		})
		api.GET("/storage", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"type": state.Repo.StorageType()}) })
		api.GET("/status", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok", "size": state.Runtime.Size(), "revision": state.Runtime.Revision()})
		})
		api.POST("/sync", func(c *gin.Context) {
			if err := state.Runtime.Sync(c.Request.Context()); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"changed": true, "revision": state.Runtime.Revision()})
		})

		api.GET("/tokens", func(c *gin.Context) {
			page, err := state.Repo.ListAccounts(c.Request.Context(), account.ListQuery{Page: 1, PageSize: 2000, IncludeDeleted: false, SortBy: "updated_at", SortDesc: true})
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			tokens := make([]map[string]any, 0, len(page.Items))
			for _, item := range page.Items {
				tokens = append(tokens, serializeToken(item))
			}
			c.JSON(http.StatusOK, gin.H{"tokens": tokens})
		})

		api.POST("/tokens", func(c *gin.Context) {
			var payload map[string][]map[string]any
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
				return
			}
			total := 0
			for pool, items := range payload {
				upserts := make([]account.Upsert, 0, len(items))
				for _, item := range items {
					token, _ := item["token"].(string)
					upserts = append(upserts, account.Upsert{
						Token: token,
						Pool:  pool,
						Tags:  normalizeAnyStringSlice(item["tags"]),
					})
				}
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
			upserts := make([]account.Upsert, 0, len(payload.Tokens))
			for _, token := range payload.Tokens {
				upserts = append(upserts, account.Upsert{Token: token, Pool: payload.Pool, Tags: payload.Tags})
			}
			result, err := state.Repo.UpsertAccounts(c.Request.Context(), upserts)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			_ = state.Runtime.Sync(c.Request.Context())
			go state.Refresh.RefreshOnImport(context.Background(), payload.Tokens)
			c.JSON(http.StatusOK, gin.H{"status": "success", "count": result.Upserted, "skipped": 0, "synced": true})
		})

		api.DELETE("/tokens", func(c *gin.Context) {
			var tokens []string
			if err := c.ShouldBindJSON(&tokens); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
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
			records, err := state.Repo.GetAccounts(c.Request.Context(), []string{payload.OldToken})
			if err != nil || len(records) == 0 {
				c.JSON(http.StatusNotFound, gin.H{"message": "Account not found"})
				return
			}
			upsert := account.Upsert{Token: payload.Token, Pool: payload.Pool, Tags: records[0].Tags, Ext: records[0].Ext}
			if _, err := state.Repo.UpsertAccounts(c.Request.Context(), []account.Upsert{upsert}); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
				return
			}
			if payload.OldToken != payload.Token {
				_, _ = state.Repo.DeleteAccounts(c.Request.Context(), []string{payload.OldToken})
			}
			_ = state.Runtime.Sync(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"status": "success", "token": payload.Token, "pool": payload.Pool})
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
		"last_used_at": item.LastUseAt,
		"tags":         item.Tags,
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
	var payload struct {
		Tokens []string `json:"tokens"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
		return
	}
	tokens := payload.Tokens
	if len(tokens) == 0 {
		tokens = state.Runtime.TokensByPool("basic")
		tokens = append(tokens, state.Runtime.TokensByPool("super")...)
		tokens = append(tokens, state.Runtime.TokensByPool("heavy")...)
	}
	asyncMode := c.Query("async") == "true"
	if asyncMode {
		task := state.Tasks.Create(len(tokens))
		go runBatchTask(task, state, kind, enabled, tokens)
		c.JSON(http.StatusOK, gin.H{"status": "success", "task_id": task.ID, "total": len(tokens)})
		return
	}
	result := runBatch(context.Background(), state, kind, enabled, tokens)
	c.JSON(http.StatusOK, gin.H{"status": "success", "summary": gin.H{"total": len(tokens), "ok": result.ok, "fail": result.fail}, "results": result.items})
}

type batchResult struct {
	ok    int
	fail  int
	items map[string]any
}

func runBatchTask(task *tasks.Task, state *app.State, kind string, enabled bool, tokens []string) {
	result := batchResult{items: map[string]any{}}
	for _, token := range tokens {
		if task.Cancelled {
			task.FinishCancelled()
			return
		}
		item, err := executeBatchItem(context.Background(), state, kind, enabled, token)
		if err != nil {
			result.fail++
			result.items[token] = gin.H{"error": err.Error()}
			task.Record(false, token, nil, err.Error())
			continue
		}
		result.ok++
		result.items[token] = item
		task.Record(true, token, item, "")
	}
	task.Finish(map[string]any{
		"status":  "success",
		"summary": map[string]any{"total": len(tokens), "ok": result.ok, "fail": result.fail},
		"results": result.items,
	}, "")
}

func runBatch(ctx context.Context, state *app.State, kind string, enabled bool, tokens []string) batchResult {
	result := batchResult{items: map[string]any{}}
	for _, token := range tokens {
		item, err := executeBatchItem(ctx, state, kind, enabled, token)
		if err != nil {
			result.fail++
			result.items[token] = gin.H{"error": err.Error()}
			continue
		}
		result.ok++
		result.items[token] = item
	}
	return result
}

func executeBatchItem(ctx context.Context, state *app.State, kind string, enabled bool, token string) (map[string]any, error) {
	switch kind {
	case "refresh":
		result, err := state.Refresh.RefreshTokens(ctx, []string{token})
		if err != nil {
			return nil, err
		}
		if result.Refreshed == 0 {
			return nil, errors.New("未获取到真实配额数据")
		}
		return map[string]any{"refreshed": result.Refreshed}, nil
	case "nsfw":
		if enabled {
			if err := state.XAI.SetBirthDate(ctx, token); err != nil {
				return nil, err
			}
		}
		if err := state.XAI.SetNSFW(ctx, token, enabled); err != nil {
			return nil, err
		}
		addTags := []string{"nsfw"}
		removeTags := []string{}
		if !enabled {
			addTags, removeTags = nil, []string{"nsfw"}
		}
		_, _ = state.Repo.PatchAccounts(ctx, []account.Patch{{Token: token, AddTags: addTags, RemoveTags: removeTags}})
		_ = state.Runtime.Sync(ctx)
		return map[string]any{"success": true, "tagged": enabled}, nil
	case "cache-clear":
		items, err := state.XAI.ListAssets(ctx, token)
		if err != nil {
			return nil, err
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
			if err := state.XAI.DeleteAsset(ctx, token, assetID); err == nil {
				deleted++
			}
		}
		return map[string]any{"deleted": deleted}, nil
	default:
		return nil, errors.New("unsupported batch operation")
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
		records = append(records, gin.H{"name": item.Name(), "size_bytes": info.Size(), "modified_at": info.ModTime().UnixMilli()})
	}
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

func ptrString(value string) *string                 { return &value }
func ptrStatus(value account.Status) *account.Status { return &value }
