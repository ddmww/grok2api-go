package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/model"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/auth"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/platform/upstreamblocker"
	"github.com/gin-gonic/gin"
)

type chatRequest struct {
	Model           string           `json:"model"`
	Messages        []map[string]any `json:"messages"`
	Stream          bool             `json:"stream"`
	Tools           []map[string]any `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
	Temperature     *float64         `json:"temperature"`
	TopP            *float64         `json:"top_p"`
	ReasoningEffort string           `json:"reasoning_effort"`
	ImageConfig     *imageConfig     `json:"image_config"`
	VideoConfig     *videoConfig     `json:"video_config"`
}

type responsesRequest struct {
	Model        string           `json:"model"`
	Input        any              `json:"input"`
	Instructions string           `json:"instructions"`
	Stream       bool             `json:"stream"`
	Tools        []map[string]any `json:"tools"`
	ToolChoice   any              `json:"tool_choice"`
}

type streamResult struct {
	content       string
	reasoning     string
	toolCalls     []ParsedToolCall
	annotations   []map[string]any
	searchSources []map[string]any
	usage         map[string]any
}

func Mount(router *gin.Engine, state *app.State) {
	v1Public := router.Group("/v1")
	{
		v1Public.GET("/files/image", func(c *gin.Context) {
			id := strings.TrimSpace(c.Query("id"))
			path, contentType := localFilePath(paths.ImageCacheDir(), id)
			if path == "" {
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "image not found", "type": "invalid_request_error"}})
				return
			}
			c.Header("Content-Type", contentType)
			c.File(path)
		})

		v1Public.GET("/files/video", func(c *gin.Context) {
			id := strings.TrimSpace(c.Query("id"))
			path, contentType := localFilePath(paths.VideoCacheDir(), id)
			if path == "" {
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "video not found", "type": "invalid_request_error"}})
				return
			}
			c.Header("Content-Type", contentType)
			c.File(path)
		})
	}

	v1 := router.Group("/v1")
	v1.Use(auth.APIKey(state.Config))
	{
		v1.GET("/models", func(c *gin.Context) {
			pools := state.Runtime.Pools()
			out := []map[string]any{}
			for _, spec := range model.All() {
				if availableForPools(spec, pools) {
					out = append(out, map[string]any{
						"id":       spec.Name,
						"object":   "model",
						"created":  time.Now().Unix(),
						"owned_by": "xai",
						"name":     spec.PublicName,
					})
				}
			}
			c.JSON(http.StatusOK, gin.H{"object": "list", "data": out})
		})

		v1.GET("/models/:id", func(c *gin.Context) {
			spec, ok := model.Get(c.Param("id"))
			if !ok || !availableForPools(spec, state.Runtime.Pools()) {
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": fmt.Sprintf("Model %q not found", c.Param("id")), "type": "invalid_request_error"}})
				return
			}
			c.JSON(http.StatusOK, map[string]any{
				"id":       spec.Name,
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "xai",
				"name":     spec.PublicName,
			})
		})

		v1.POST("/chat/completions", func(c *gin.Context) {
			var request chatRequest
			if err := c.ShouldBindJSON(&request); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			spec, ok := model.Get(request.Model)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "unknown model", "type": "invalid_request_error"}})
				return
			}
			if spec.IsImage() {
				cfg, err := normalizeImageConfig(derefImageConfig(request.ImageConfig), spec.Name, false)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
					return
				}
				result, err := generateImages(c.Request.Context(), state, spec, extractPromptFromMessages(request.Messages), cfg, true)
				if err != nil {
					writeOpenAIError(c, err)
					return
				}
				c.JSON(http.StatusOK, result)
				return
			}
			if spec.IsImageEdit() {
				cfg, err := normalizeImageConfig(derefImageConfig(request.ImageConfig), spec.Name, true)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
					return
				}
				result, err := editImages(c.Request.Context(), state, spec, request.Messages, cfg, true)
				if err != nil {
					writeOpenAIError(c, err)
					return
				}
				c.JSON(http.StatusOK, result)
				return
			}
			if spec.IsVideo() {
				cfg, err := normalizeVideoConfig(derefVideoConfig(request.VideoConfig))
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
					return
				}
				result, err := createVideo(c.Request.Context(), state, spec, extractPromptFromMessages(request.Messages), cfg)
				if err != nil {
					writeOpenAIError(c, err)
					return
				}
				c.JSON(http.StatusOK, videoChatResponse(state, spec, result))
				return
			}
			if request.Stream {
				streamChat(c, state, spec, request)
				return
			}
			result, err := runChat(c.Request.Context(), state, spec, request.Messages, request.Tools, request.ToolChoice)
			if err != nil {
				writeOpenAIError(c, err)
				return
			}
			if err := upstreamblocker.AssertResponseAllowed(upstreamblocker.GetConfig(state.Config), result.content, "/v1/chat/completions"); err != nil {
				writeOpenAIError(c, err)
				return
			}
			if len(result.toolCalls) > 0 {
				c.JSON(http.StatusOK, chatToolResponse(spec.Name, result.toolCalls, request.Messages, result.usage))
				return
			}
			if !state.Config.GetBool("features.thinking", true) {
				result.reasoning = ""
			}
			response := chatResponse(spec.Name, result.content, result.reasoning, request.Messages, result.usage)
			if choices, ok := response["choices"].([]map[string]any); ok && len(choices) > 0 {
				if message, ok := choices[0]["message"].(map[string]any); ok {
					if len(result.annotations) > 0 {
						message["annotations"] = result.annotations
					}
				}
			}
			if len(result.searchSources) > 0 {
				response["search_sources"] = result.searchSources
			}
			c.JSON(http.StatusOK, response)
		})

		v1.POST("/responses", func(c *gin.Context) {
			var request responsesRequest
			if err := c.ShouldBindJSON(&request); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			spec, ok := model.Get(request.Model)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "unknown model", "type": "invalid_request_error"}})
				return
			}
			messages := parseResponsesInput(request.Input)
			if request.Instructions != "" {
				messages = append([]map[string]any{{"role": "system", "content": request.Instructions}}, messages...)
			}
			if request.Stream {
				streamResponses(c, state, spec, messages, request.Tools, request.ToolChoice)
				return
			}
			result, err := runChat(c.Request.Context(), state, spec, messages, request.Tools, request.ToolChoice)
			if err != nil {
				writeOpenAIError(c, err)
				return
			}
			if err := upstreamblocker.AssertResponseAllowed(upstreamblocker.GetConfig(state.Config), result.content, "/v1/responses"); err != nil {
				writeOpenAIError(c, err)
				return
			}
			respID := responseID("resp")
			if len(result.toolCalls) > 0 {
				output := []map[string]any{}
				for _, call := range result.toolCalls {
					output = append(output, map[string]any{
						"id":        responseID("fc"),
						"type":      "function_call",
						"call_id":   call.CallID,
						"name":      call.Name,
						"arguments": call.Arguments,
						"status":    "completed",
					})
				}
				response := responsesObject(respID, spec.Name, "completed", output, responsesToolUsageOrEstimate(spec.Name, result.usage, messages, len(output)))
				if len(result.searchSources) > 0 {
					response["search_sources"] = result.searchSources
				}
				c.JSON(http.StatusOK, response)
				return
			}
			contentItem := map[string]any{
				"type": "output_text",
				"text": result.content,
			}
			if len(result.annotations) > 0 {
				contentItem["annotations"] = result.annotations
			}
			output := []map[string]any{{
				"id":      responseID("msg"),
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{contentItem},
			}}
			response := responsesObject(respID, spec.Name, "completed", output, responsesUsageOrEstimate(spec.Name, result.usage, messages, result.content, result.reasoning))
			if len(result.searchSources) > 0 {
				response["search_sources"] = result.searchSources
			}
			c.JSON(http.StatusOK, response)
		})

		v1.POST("/images/generations", func(c *gin.Context) {
			var payload struct {
				Model          string `json:"model"`
				Prompt         string `json:"prompt"`
				N              int    `json:"n"`
				Size           string `json:"size"`
				ResponseFormat string `json:"response_format"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			spec, ok := model.Get(payload.Model)
			if !ok || !spec.IsImage() {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Model %q is not an image model", payload.Model), "type": "invalid_request_error"}})
				return
			}
			cfg, err := normalizeImageConfig(imageConfig{N: payload.N, Size: payload.Size, ResponseFormat: payload.ResponseFormat}, spec.Name, false)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			result, err := generateImages(c.Request.Context(), state, spec, payload.Prompt, cfg, false)
			if err != nil {
				writeOpenAIError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
		})

		v1.POST("/images/edits", func(c *gin.Context) {
			modelName := strings.TrimSpace(c.PostForm("model"))
			prompt := c.PostForm("prompt")
			n, _ := strconv.Atoi(defaultIfEmpty(c.PostForm("n"), "1"))
			spec, ok := model.Get(modelName)
			if !ok || !spec.IsImageEdit() {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Model %q is not an image-edit model", modelName), "type": "invalid_request_error"}})
				return
			}
			if strings.TrimSpace(c.PostForm("mask")) != "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "mask is not supported yet", "type": "invalid_request_error"}})
				return
			}
			cfg, err := normalizeImageConfig(imageConfig{N: n, Size: defaultIfEmpty(c.PostForm("size"), "1024x1024"), ResponseFormat: defaultIfEmpty(c.PostForm("response_format"), "url")}, spec.Name, true)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			form, err := c.MultipartForm()
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			files := append(form.File["image[]"], form.File["image"]...)
			content := []map[string]any{{"type": "text", "text": prompt}}
			for _, file := range files {
				handle, err := file.Open()
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
					return
				}
				data, readErr := io.ReadAll(handle)
				_ = handle.Close()
				if readErr != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": readErr.Error(), "type": "invalid_request_error"}})
					return
				}
				mimeType := file.Header.Get("Content-Type")
				if mimeType == "" {
					mimeType = "image/png"
				}
				content = append(content, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)},
				})
			}
			result, err := editImages(c.Request.Context(), state, spec, []map[string]any{{"role": "user", "content": content}}, cfg, false)
			if err != nil {
				writeOpenAIError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
		})

		v1.POST("/videos", func(c *gin.Context) {
			modelName := defaultIfEmpty(strings.TrimSpace(c.PostForm("model")), "grok-imagine-video")
			spec, ok := model.Get(modelName)
			if !ok || !spec.IsVideo() {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Model %q is not a video model", modelName), "type": "invalid_request_error"}})
				return
			}
			seconds, _ := strconv.Atoi(defaultIfEmpty(c.PostForm("seconds"), "6"))
			cfg, err := normalizeVideoConfig(videoConfig{
				Seconds:        seconds,
				Size:           defaultIfEmpty(c.PostForm("size"), "720x1280"),
				ResolutionName: c.PostForm("resolution_name"),
				Preset:         c.PostForm("preset"),
			})
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
				return
			}
			result, err := createVideo(c.Request.Context(), state, spec, c.PostForm("prompt"), cfg)
			if err != nil {
				writeOpenAIError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
		})

		v1.GET("/videos/:id", func(c *gin.Context) {
			job := getVideoJob(c.Param("id"))
			if job == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "video not found", "type": "invalid_request_error"}})
				return
			}
			c.JSON(http.StatusOK, job.toMap())
		})

		v1.GET("/videos/:id/content", func(c *gin.Context) {
			job := getVideoJob(c.Param("id"))
			if job == nil || job.ContentPath == "" {
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "video content not found", "type": "invalid_request_error"}})
				return
			}
			c.File(job.ContentPath)
		})

	}
}

func derefImageConfig(cfg *imageConfig) imageConfig {
	if cfg == nil {
		return imageConfig{}
	}
	return *cfg
}

func derefVideoConfig(cfg *videoConfig) videoConfig {
	if cfg == nil {
		return videoConfig{}
	}
	return *cfg
}

func defaultIfEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func availableForPools(spec model.Spec, pools []string) bool {
	for _, candidate := range spec.PoolCandidates() {
		for _, pool := range pools {
			if candidate == pool {
				return true
			}
		}
	}
	return false
}

func writeOpenAIError(c *gin.Context, err error) {
	message := err.Error()
	status := httpStatusForError(err)
	errorType := openAIErrorType(status)
	errorCode := ""
	if blocked, ok := err.(*upstreamblocker.Error); ok {
		message = blocked.Error()
		errorType = "upstream_blocked"
		errorCode = "upstream_blocked"
	}
	if upstream, ok := err.(*xai.UpstreamError); ok && upstream.Status == http.StatusForbidden && strings.TrimSpace(upstream.Body) == "" {
		message = "Upstream returned 403 (challenge or permission denied)"
	}
	body := gin.H{"message": message, "type": errorType}
	if errorCode != "" {
		body["code"] = errorCode
	}
	c.JSON(status, gin.H{"error": body})
}

func openAIErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

func httpStatusForError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if upstream, ok := err.(*xai.UpstreamError); ok {
		switch upstream.Status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
			return upstream.Status
		default:
			if upstream.Status >= 500 && upstream.Status <= 599 {
				return http.StatusBadGateway
			}
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "no available accounts") {
		return http.StatusTooManyRequests
	}
	if _, ok := err.(*upstreamblocker.Error); ok {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func parseRetryCodes(raw string) map[int]struct{} {
	out := map[int]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil {
			out[value] = struct{}{}
		}
	}
	return out
}

func retryCodesFromConfig(state *app.State) map[int]struct{} {
	if state == nil || state.Config == nil {
		return parseRetryCodes("429,401,502,503")
	}
	if raw := strings.TrimSpace(state.Config.GetString("retry.on_codes", "")); raw != "" {
		return parseRetryCodes(raw)
	}
	out := map[int]struct{}{}
	addCodes := func(values []string) {
		for _, value := range values {
			code, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil {
				out[code] = struct{}{}
			}
		}
	}
	addCodes(state.Config.GetStringSlice("retry.retry_status_codes"))
	addCodes(state.Config.GetStringSlice("retry.token_switch_status_codes"))
	if len(out) == 0 {
		return parseRetryCodes("429,401,502,503")
	}
	return out
}

func maxRetriesFromConfig(state *app.State) int {
	if state == nil || state.Config == nil {
		return 1
	}
	if value := state.Config.GetInt("retry.max_retries", -1); value >= 0 {
		return maxInt(value, 0)
	}
	return maxInt(state.Config.GetInt("retry.max_retry", 1), 0)
}

func shouldRetry(err error, retryCodes map[int]struct{}, attempt, maxRetries int) bool {
	if attempt >= maxRetries {
		return false
	}
	upstream, ok := err.(*xai.UpstreamError)
	if !ok {
		return false
	}
	if isInvalidCredentialsError(upstream) {
		return true
	}
	_, retry := retryCodes[upstream.Status]
	return retry
}

func reserveLease(state *app.State, spec model.Spec, excluded map[string]struct{}) (*account.Lease, error) {
	tryReserve := func() (*account.Lease, error) {
		autoFallback := true
		if state != nil && state.Config != nil {
			autoFallback = state.Config.GetBool("features.auto_chat_mode_fallback", true)
		}
		var lastErr error
		for _, mode := range model.ModeCandidates(spec, autoFallback) {
			lease, err := state.Runtime.ReserveWithExclude(spec.WithMode(mode), excluded)
			if err == nil {
				return lease, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	lease, err := tryReserve()
	if err == nil {
		return lease, nil
	}
	if state == nil || state.Refresh == nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, refreshErr := state.Refresh.RefreshOnDemand(ctx); refreshErr != nil {
		return nil, err
	}
	return tryReserve()
}

func feedbackForError(err error) account.Feedback {
	feedback := account.Feedback{Kind: account.FeedbackServerError, Reason: err.Error()}
	if upstream, ok := err.(*xai.UpstreamError); ok {
		switch upstream.Status {
		case http.StatusUnauthorized:
			feedback.Kind = account.FeedbackUnauthorized
		case http.StatusForbidden:
			feedback.Kind = account.FeedbackForbidden
		case http.StatusTooManyRequests:
			feedback.Kind = account.FeedbackRateLimited
		}
	}
	return feedback
}

func syncUsedQuotaAsync(state *app.State, token, mode string) {
	if state == nil || state.Refresh == nil || strings.TrimSpace(token) == "" || strings.TrimSpace(mode) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = state.Refresh.RefreshCall(ctx, token, mode)
	}()
}

func buildReversePayload(state *app.State, spec model.Spec, message string) map[string]any {
	modelName, modelMode := upstreamChatModel(spec)
	return map[string]any{
		"deviceEnvInfo":               map[string]any{"darkModeEnabled": false, "devicePixelRatio": 2, "screenHeight": 1329, "screenWidth": 2056, "viewportHeight": 1083, "viewportWidth": 2056},
		"disableMemory":               !state.Config.GetBool("features.memory", false),
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             []string{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []string{},
		"imageGenerationCount":        2,
		"isAsyncChat":                 false,
		"isReasoning":                 false,
		"message":                     message,
		"modelMode":                   modelMode,
		"modelName":                   modelName,
		"responseMetadata":            map[string]any{"requestModelDetails": map[string]any{"modelId": modelName}},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"sendFinalMetadata":           true,
		"temporary":                   state.Config.GetBool("features.temporary", true),
		"toolOverrides":               map[string]any{},
	}
}

func upstreamChatModel(spec model.Spec) (string, string) {
	modelName := spec.Name
	switch {
	case strings.HasPrefix(spec.Name, "grok-4.20"):
		modelName = "grok-420"
	case strings.HasPrefix(spec.Name, "grok-4.3"):
		modelName = "grok-4-3"
	}
	modelMode := "MODEL_MODE_FAST"
	switch spec.Mode {
	case "auto":
		if modelName == "grok-420" {
			modelMode = "MODEL_MODE_GROK_420"
		} else {
			modelMode = "MODEL_MODE_AUTO"
		}
	case "expert":
		modelMode = "MODEL_MODE_EXPERT"
	case "heavy":
		modelMode = "MODEL_MODE_HEAVY"
	case "grok-420-computer-use-sa":
		modelMode = "MODEL_MODE_GROK_4_3"
	}
	return modelName, modelMode
}

func prepareMessage(messages []map[string]any, tools []map[string]any, toolChoice any) (string, []string) {
	message := flattenMessages(messages)
	toolNames := []string{}
	if len(tools) > 0 {
		message = injectToolPrompt(message, buildToolSystemPrompt(tools, toolChoice))
		toolNames = extractToolNames(tools)
	}
	return message, toolNames
}

func runChat(ctx context.Context, state *app.State, spec model.Spec, messages []map[string]any, tools []map[string]any, toolChoice any) (streamResult, error) {
	timeoutSec := state.Config.GetInt("chat.timeout", 60)
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	message, toolNames := prepareMessage(messages, tools, toolChoice)
	retryCodes := retryCodesFromConfig(state)
	maxRetries := maxRetriesFromConfig(state)
	excluded := map[string]struct{}{}
	var lastRetryErr error
	session, err := state.XAI.NewChatSession()
	if err != nil {
		return streamResult{}, err
	}
	defer session.Close()

	for attempt := 0; attempt <= maxRetries; attempt++ {
		lease, err := reserveLease(state, spec, excluded)
		if err != nil {
			if lastRetryErr != nil {
				return streamResult{}, lastRetryErr
			}
			return streamResult{}, err
		}

		lines, errCh := session.ChatStream(ctx, lease.Token, buildReversePayload(state, spec, message))
		result := streamResult{}
		adapter := xai.NewStreamAdapter(state.Config)
		for line := range lines {
			kind, data := xai.ClassifyLine(line)
			if kind != "data" {
				continue
			}
			for _, event := range adapter.Feed(data) {
				switch event.Kind {
				case "thinking":
					result.reasoning += event.Content
				case "text":
					result.content += event.Content
				case "annotation":
					if event.AnnotationData != nil {
						result.annotations = append(result.annotations, event.AnnotationData)
					}
				}
			}
		}
		if trailing := adapter.FinalizeThinking(); trailing != "" {
			result.reasoning += trailing
		}

		if err := <-errCh; err != nil {
			_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
			if shouldRetry(err, retryCodes, attempt, maxRetries) {
				lastRetryErr = err
				excluded[lease.Token] = struct{}{}
				continue
			}
			return streamResult{}, err
		}

		if len(toolNames) > 0 {
			result.toolCalls = parseToolCalls(result.content, toolNames)
			if len(result.toolCalls) > 0 {
				result.content = ""
			}
		}
		if strings.TrimSpace(result.content) == "" {
			result.content = adapter.FinalText()
		}
		if strings.TrimSpace(result.content) == "" && len(result.toolCalls) == 0 {
			if errorMessage := adapter.FinalError(); errorMessage != "" {
				err := &xai.UpstreamError{Status: http.StatusServiceUnavailable, Body: errorMessage}
				_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
				if shouldRetry(err, retryCodes, attempt, maxRetries) {
					lastRetryErr = err
					excluded[lease.Token] = struct{}{}
					continue
				}
				return streamResult{}, err
			}
		}
		result.searchSources = adapter.SearchSourcesList()
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
		syncUsedQuotaAsync(state, lease.Token, lease.Mode)
		return result, nil
	}

	return streamResult{}, fmt.Errorf("no available accounts for this model tier")
}

func writeSSEData(c *gin.Context, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := c.Writer.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
		return false
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeSSEEvent(c *gin.Context, event string, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := c.Writer.Write([]byte("event: " + event + "\n" + "data: " + string(data) + "\n\n")); err != nil {
		return false
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeSSEDone(c *gin.Context) bool {
	if _, err := c.Writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		return false
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func finalTextCompletionDelta(result *streamResult, adapter *xai.StreamAdapter) string {
	finalText := adapter.FinalText()
	if finalText == "" {
		return ""
	}
	if result.content == "" {
		result.content = finalText
		return finalText
	}
	if strings.HasPrefix(finalText, result.content) && len(finalText) > len(result.content) {
		delta := finalText[len(result.content):]
		result.content = finalText
		return delta
	}
	return ""
}

func classifyLine(line string) (string, string) {
	return xai.ClassifyLine(line)
}

func parseChatData(data string) (content, reasoning string, stop bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return "", "", false
	}
	result, _ := payload["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return "", "", false
	}
	if finalMetadata := response["finalMetadata"]; finalMetadata != nil {
		return "", "", true
	}
	if isSoftStop, _ := response["isSoftStop"].(bool); isSoftStop {
		return "", "", true
	}
	token, _ := response["token"].(string)
	isThinking, _ := response["isThinking"].(bool)
	messageTag, _ := response["messageTag"].(string)
	if token != "" && isThinking {
		return "", token, false
	}
	if token != "" && messageTag == "final" {
		return token, "", false
	}
	return "", "", false
}

func flattenMessages(messages []map[string]any) string {
	parts := []string{}
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		content := msg["content"]
		if role == "tool" {
			toolCallID, _ := msg["tool_call_id"].(string)
			parts = append(parts, fmt.Sprintf("[tool result for %s]:\n%s", toolCallID, stringifyContent(content)))
			continue
		}
		if role == "assistant" {
			if raw, ok := msg["tool_calls"].([]any); ok && len(raw) > 0 {
				toolCalls := []map[string]any{}
				for _, item := range raw {
					if mapped, ok := item.(map[string]any); ok {
						toolCalls = append(toolCalls, mapped)
					}
				}
				parts = append(parts, "[assistant]:\n"+toolCallsToXML(toolCalls))
				continue
			}
		}
		text := stringifyContent(content)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, fmt.Sprintf("[%s]: %s", role, text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func stringifyContent(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []map[string]any:
		asAny := make([]any, 0, len(typed))
		for _, item := range typed {
			asAny = append(asAny, item)
		}
		return stringifyContent(asAny)
	case []any:
		parts := []string{}
		for _, item := range typed {
			mapped, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch mapped["type"] {
			case "text":
				if text, _ := mapped["text"].(string); text != "" {
					parts = append(parts, text)
				}
			case "input_text", "output_text":
				if text, _ := mapped["text"].(string); text != "" {
					parts = append(parts, text)
				}
			case "image_url":
				imageURL, _ := mapped["image_url"].(map[string]any)
				if url, _ := imageURL["url"].(string); url != "" {
					parts = append(parts, url)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func parseResponsesInput(input any) []map[string]any {
	if text, ok := input.(string); ok {
		return []map[string]any{{"role": "user", "content": text}}
	}
	raw, ok := input.([]any)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, item := range raw {
		mapped, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch mapped["type"] {
		case "function_call":
			out = append(out, map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   mapped["call_id"],
					"type": "function",
					"function": map[string]any{
						"name":      mapped["name"],
						"arguments": mapped["arguments"],
					},
				}},
			})
		case "function_call_output":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": mapped["call_id"],
				"content":      mapped["output"],
			})
		default:
			role, _ := mapped["role"].(string)
			out = append(out, map[string]any{"role": role, "content": mapped["content"]})
		}
	}
	return out
}

func streamChat(c *gin.Context, state *app.State, spec model.Spec, request chatRequest) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	id := responseID("chatcmpl")
	message, toolNames := prepareMessage(request.Messages, request.Tools, request.ToolChoice)
	retryCodes := retryCodesFromConfig(state)
	maxRetries := maxRetriesFromConfig(state)
	excluded := map[string]struct{}{}
	thinkingEnabled := state.Config.GetBool("features.thinking", true)
	session, err := state.XAI.NewChatSession()
	if err != nil {
		writeOpenAIError(c, err)
		return
	}
	defer session.Close()

	for attempt := 0; attempt <= maxRetries; attempt++ {
		lease, err := reserveLease(state, spec, excluded)
		if err != nil {
			writeOpenAIError(c, err)
			return
		}

		lines, errCh := session.ChatStream(c.Request.Context(), lease.Token, buildReversePayload(state, spec, message))
		result := streamResult{}
		adapter := xai.NewStreamAdapter(state.Config)
		outputStarted := false

		for line := range lines {
			kind, data := xai.ClassifyLine(line)
			if kind != "data" {
				continue
			}
			for _, event := range adapter.Feed(data) {
				switch event.Kind {
				case "thinking":
					result.reasoning += event.Content
					if len(toolNames) == 0 && thinkingEnabled {
						outputStarted = true
						if !writeSSEData(c, map[string]any{
							"id":      id,
							"object":  "chat.completion.chunk",
							"created": time.Now().Unix(),
							"model":   spec.Name,
							"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": event.Content}}},
						}) {
							return
						}
					}
				case "text":
					result.content += event.Content
					if len(toolNames) == 0 {
						outputStarted = true
						if !writeSSEData(c, map[string]any{
							"id":      id,
							"object":  "chat.completion.chunk",
							"created": time.Now().Unix(),
							"model":   spec.Name,
							"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": event.Content}}},
						}) {
							return
						}
					}
				case "annotation":
					if event.AnnotationData != nil {
						result.annotations = append(result.annotations, event.AnnotationData)
					}
				}
			}
		}
		if trailing := adapter.FinalizeThinking(); trailing != "" {
			result.reasoning += trailing
		}

		if err := <-errCh; err != nil {
			_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
			if !outputStarted && shouldRetry(err, retryCodes, attempt, maxRetries) {
				excluded[lease.Token] = struct{}{}
				continue
			}
			if !outputStarted {
				writeOpenAIError(c, err)
			}
			return
		}

		if delta := finalTextCompletionDelta(&result, adapter); delta != "" && len(toolNames) == 0 {
			outputStarted = true
			if !writeSSEData(c, map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   spec.Name,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": delta}}},
			}) {
				return
			}
		}

		if len(toolNames) > 0 {
			if finalText := adapter.FinalText(); result.content == "" && finalText != "" {
				result.content = finalText
			}
			result.toolCalls = parseToolCalls(result.content, toolNames)
			if len(result.toolCalls) > 0 {
				for index, call := range result.toolCalls {
					if !writeSSEData(c, map[string]any{
						"id":      id,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   spec.Name,
						"choices": []map[string]any{{
							"index": 0,
							"delta": map[string]any{
								"role":    "assistant",
								"content": nil,
								"tool_calls": []map[string]any{{
									"index": index,
									"id":    call.CallID,
									"type":  "function",
									"function": map[string]any{
										"name":      call.Name,
										"arguments": call.Arguments,
									},
								}},
							},
						}},
					}) {
						return
					}
				}
				_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
				syncUsedQuotaAsync(state, lease.Token, lease.Mode)
				payload := map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   spec.Name,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
				}
				if sources := adapter.SearchSourcesList(); len(sources) > 0 {
					payload["search_sources"] = sources
				}
				_ = writeSSEData(c, payload)
				_ = writeSSEDone(c)
				return
			}
			if thinkingEnabled && result.reasoning != "" {
				if !writeSSEData(c, map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   spec.Name,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": result.reasoning}}},
				}) {
					return
				}
			}
			if result.content != "" {
				if !writeSSEData(c, map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   spec.Name,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": result.content}}},
				}) {
					return
				}
			}
		}
		result.searchSources = adapter.SearchSourcesList()

		_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
		syncUsedQuotaAsync(state, lease.Token, lease.Mode)
		finalChunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   spec.Name,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   chatUsageOrEstimate(spec.Name, result.usage, request.Messages, result.content, result.reasoning),
		}
		if len(result.annotations) > 0 {
			finalChunk["annotations"] = result.annotations
		}
		if len(result.searchSources) > 0 {
			finalChunk["search_sources"] = result.searchSources
		}
		_ = writeSSEData(c, finalChunk)
		_ = writeSSEDone(c)
		return
	}
}

func streamResponses(c *gin.Context, state *app.State, spec model.Spec, messages []map[string]any, tools []map[string]any, toolChoice any) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	id := responseID("resp")
	message, toolNames := prepareMessage(messages, tools, toolChoice)
	retryCodes := retryCodesFromConfig(state)
	maxRetries := maxRetriesFromConfig(state)
	excluded := map[string]struct{}{}
	session, err := state.XAI.NewChatSession()
	if err != nil {
		writeOpenAIError(c, err)
		return
	}
	defer session.Close()
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lease, err := reserveLease(state, spec, excluded)
		if err != nil {
			writeOpenAIError(c, err)
			return
		}

		lines, errCh := session.ChatStream(c.Request.Context(), lease.Token, buildReversePayload(state, spec, message))
		result := streamResult{}
		adapter := xai.NewStreamAdapter(state.Config)
		itemID := responseID("msg")
		createdSent := false

		sendCreated := func() bool {
			if createdSent {
				return true
			}
			createdSent = true
			return writeSSEEvent(c, "response.created", map[string]any{
				"type":     "response.created",
				"response": responsesObject(id, spec.Name, "in_progress", []map[string]any{}, nil),
			}) && writeSSEEvent(c, "response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item": map[string]any{
					"id":      itemID,
					"type":    "message",
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": ""}},
				},
			})
		}

		for line := range lines {
			kind, data := xai.ClassifyLine(line)
			if kind != "data" {
				continue
			}
			for _, event := range adapter.Feed(data) {
				switch event.Kind {
				case "thinking":
					result.reasoning += event.Content
				case "text":
					result.content += event.Content
					if len(toolNames) == 0 {
						if !sendCreated() {
							return
						}
						if !writeSSEEvent(c, "response.output_text.delta", map[string]any{
							"type":          "response.output_text.delta",
							"item_id":       itemID,
							"output_index":  0,
							"content_index": 0,
							"delta":         event.Content,
						}) {
							return
						}
					}
				case "annotation":
					if event.AnnotationData != nil {
						result.annotations = append(result.annotations, event.AnnotationData)
					}
				}
			}
		}

		if err := <-errCh; err != nil {
			_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
			if !createdSent && shouldRetry(err, retryCodes, attempt, maxRetries) {
				excluded[lease.Token] = struct{}{}
				continue
			}
			if !createdSent {
				writeOpenAIError(c, err)
			}
			return
		}
		result.searchSources = adapter.SearchSourcesList()

		if delta := finalTextCompletionDelta(&result, adapter); delta != "" && len(toolNames) == 0 {
			if !sendCreated() {
				return
			}
			if !writeSSEEvent(c, "response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       itemID,
				"output_index":  0,
				"content_index": 0,
				"delta":         delta,
			}) {
				return
			}
		}

		if len(toolNames) > 0 {
			if finalText := adapter.FinalText(); result.content == "" && finalText != "" {
				result.content = finalText
			}
			result.toolCalls = parseToolCalls(result.content, toolNames)
			if !writeSSEEvent(c, "response.created", map[string]any{
				"type":     "response.created",
				"response": responsesObject(id, spec.Name, "in_progress", []map[string]any{}, nil),
			}) {
				return
			}
			if len(result.toolCalls) > 0 {
				output := []map[string]any{}
				for index, call := range result.toolCalls {
					fcItemID := responseID("fc")
					item := map[string]any{"id": fcItemID, "type": "function_call", "call_id": call.CallID, "name": call.Name, "arguments": call.Arguments, "status": "completed"}
					output = append(output, item)
					if !writeSSEEvent(c, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"id": fcItemID, "type": "function_call", "call_id": call.CallID, "name": call.Name, "arguments": "", "status": "in_progress"}}) {
						return
					}
					if !writeSSEEvent(c, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": fcItemID, "output_index": index, "delta": call.Arguments}) {
						return
					}
					if !writeSSEEvent(c, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": fcItemID, "output_index": index, "arguments": call.Arguments}) {
						return
					}
					if !writeSSEEvent(c, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}) {
						return
					}
				}
				_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
				syncUsedQuotaAsync(state, lease.Token, lease.Mode)
				response := responsesObject(id, spec.Name, "completed", output, responsesToolUsageOrEstimate(spec.Name, result.usage, messages, len(output)))
				if sources := adapter.SearchSourcesList(); len(sources) > 0 {
					response["search_sources"] = sources
				}
				_ = writeSSEEvent(c, "response.completed", map[string]any{"type": "response.completed", "response": response})
				_ = writeSSEDone(c)
				return
			}
		}

		if !createdSent {
			if !sendCreated() {
				return
			}
		}
		contentItem := map[string]any{"type": "output_text", "text": result.content}
		if len(result.annotations) > 0 {
			contentItem["annotations"] = result.annotations
		}
		messageItem := map[string]any{"id": itemID, "type": "message", "role": "assistant", "content": []map[string]any{contentItem}}
		if !writeSSEEvent(c, "response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       itemID,
			"output_index":  0,
			"content_index": 0,
			"text":          result.content,
		}) {
			return
		}
		if !writeSSEEvent(c, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": messageItem}) {
			return
		}
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
		syncUsedQuotaAsync(state, lease.Token, lease.Mode)
		response := responsesObject(id, spec.Name, "completed", []map[string]any{messageItem}, responsesUsageOrEstimate(spec.Name, result.usage, messages, result.content, result.reasoning))
		if len(result.searchSources) > 0 {
			response["search_sources"] = result.searchSources
		}
		_ = writeSSEEvent(c, "response.completed", map[string]any{"type": "response.completed", "response": response})
		_ = writeSSEDone(c)
		return
	}
}

func isInvalidCredentialsBody(body string) bool {
	text := strings.ToLower(strings.TrimSpace(body))
	return strings.Contains(text, "invalid-credentials") ||
		strings.Contains(text, "bad-credentials") ||
		strings.Contains(text, "failed to look up session id") ||
		strings.Contains(text, "blocked-user") ||
		strings.Contains(text, "email-domain-rejected") ||
		strings.Contains(text, "session not found") ||
		strings.Contains(text, "account suspended") ||
		strings.Contains(text, "token revoked") ||
		strings.Contains(text, "token expired")
}

func isInvalidCredentialsError(err *xai.UpstreamError) bool {
	if err == nil {
		return false
	}
	if err.Status != http.StatusBadRequest && err.Status != http.StatusUnauthorized && err.Status != http.StatusForbidden {
		return false
	}
	return isInvalidCredentialsBody(err.Body)
}
