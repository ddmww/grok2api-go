package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/model"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/auth"
	"github.com/ddmww/grok2api-go/internal/platform/tokens"
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
	content   string
	reasoning string
	toolCalls []ParsedToolCall
}

func Mount(router *gin.Engine, state *app.State) {
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
			if request.Stream {
				streamChat(c, state, spec, request)
				return
			}
			result, err := runChat(c.Request.Context(), state, spec, request.Messages, request.Tools, request.ToolChoice)
			if err != nil {
				writeOpenAIError(c, err)
				return
			}
			if len(result.toolCalls) > 0 {
				c.JSON(http.StatusOK, chatToolResponse(spec.Name, result.toolCalls, request.Messages))
				return
			}
			c.JSON(http.StatusOK, chatResponse(spec.Name, result.content, result.reasoning, request.Messages))
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
			respID := responseID("resp")
			promptTokens := tokens.EstimateAny(messages)
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
				c.JSON(http.StatusOK, responsesObject(respID, spec.Name, "completed", output, responsesUsage(promptTokens, maxInt(len(output)*12, 8), 0)))
				return
			}
			output := []map[string]any{{
				"id":   responseID("msg"),
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": result.content,
				}},
			}}
			c.JSON(http.StatusOK, responsesObject(respID, spec.Name, "completed", output, responsesUsage(promptTokens, tokens.EstimateText(result.content), tokens.EstimateText(result.reasoning))))
		})
	}
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
	status := http.StatusInternalServerError
	message := err.Error()
	if upstream, ok := err.(interface{ Error() string }); ok {
		message = upstream.Error()
	}
	if strings.Contains(strings.ToLower(message), "no available accounts") {
		status = http.StatusTooManyRequests
	}
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": "server_error"}})
}

func runChat(ctx context.Context, state *app.State, spec model.Spec, messages []map[string]any, tools []map[string]any, toolChoice any) (streamResult, error) {
	lease, err := state.Runtime.Reserve(spec)
	if err != nil {
		return streamResult{}, err
	}
	defer state.Runtime.Release(lease.Token)

	message := flattenMessages(messages)
	toolNames := []string{}
	if len(tools) > 0 {
		message = injectToolPrompt(message, buildToolSystemPrompt(tools, toolChoice))
		toolNames = extractToolNames(tools)
	}
	payload := map[string]any{
		"collectionIds":               []string{},
		"connectors":                  []string{},
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
		"message":                     message,
		"modeId":                      spec.Mode,
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"searchAllConnectors":         false,
		"sendFinalMetadata":           true,
		"temporary":                   state.Config.GetBool("features.temporary", true),
		"toolOverrides": map[string]any{
			"gmailSearch":           false,
			"googleCalendarSearch":  false,
			"outlookSearch":         false,
			"outlookCalendarSearch": false,
			"googleDriveSearch":     false,
		},
	}
	lines, errCh := state.XAI.ChatStream(ctx, lease.Token, payload)
	result := streamResult{}
	for line := range lines {
		kind, data := classifyLine(line)
		if kind != "data" {
			continue
		}
		content, reasoning, stop := parseChatData(data)
		if reasoning != "" {
			result.reasoning += reasoning
		}
		if content != "" {
			result.content += content
		}
		if stop {
			break
		}
	}
	if err := <-errCh; err != nil {
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
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedback)
		return streamResult{}, err
	}
	if len(toolNames) > 0 {
		result.toolCalls = parseToolCalls(result.content, toolNames)
		if len(result.toolCalls) > 0 {
			result.content = ""
		}
	}
	_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
	return result, nil
}

func classifyLine(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "event:") {
		return "skip", ""
	}
	if strings.HasPrefix(line, "data:") {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return "done", ""
		}
		return "data", data
	}
	if strings.HasPrefix(line, "{") {
		return "data", line
	}
	return "skip", ""
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
	result, err := runChat(c.Request.Context(), state, spec, request.Messages, request.Tools, request.ToolChoice)
	if err != nil {
		writeOpenAIError(c, err)
		return
	}
	c.Stream(func(w io.Writer) bool {
		if len(result.toolCalls) > 0 {
			for index, call := range result.toolCalls {
				payload := map[string]any{
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
				}
				data, _ := json.Marshal(payload)
				_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
			}
			done, _ := json.Marshal(map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   spec.Name,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			})
			_, _ = w.Write([]byte("data: " + string(done) + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return false
		}
		if result.reasoning != "" && state.Config.GetBool("features.thinking", true) {
			data, _ := json.Marshal(map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   spec.Name,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": result.reasoning}}},
			})
			_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		}
		data, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   spec.Name,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": result.content}}},
		})
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		done, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   spec.Name,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   buildUsage(tokens.EstimateAny(request.Messages), tokens.EstimateText(result.content)+tokens.EstimateText(result.reasoning), tokens.EstimateText(result.reasoning)),
		})
		_, _ = w.Write([]byte("data: " + string(done) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return false
	})
}

func streamResponses(c *gin.Context, state *app.State, spec model.Spec, messages []map[string]any, tools []map[string]any, toolChoice any) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	id := responseID("resp")
	result, err := runChat(c.Request.Context(), state, spec, messages, tools, toolChoice)
	if err != nil {
		writeOpenAIError(c, err)
		return
	}
	c.Stream(func(w io.Writer) bool {
		created, _ := json.Marshal(map[string]any{"type": "response.created", "response": responsesObject(id, spec.Name, "in_progress", []map[string]any{}, nil)})
		_, _ = w.Write([]byte("event: response.created\ndata: " + string(created) + "\n\n"))
		promptTokens := tokens.EstimateAny(messages)
		if len(result.toolCalls) > 0 {
			output := []map[string]any{}
			for index, call := range result.toolCalls {
				itemID := responseID("fc")
				output = append(output, map[string]any{"id": itemID, "type": "function_call", "call_id": call.CallID, "name": call.Name, "arguments": call.Arguments, "status": "completed"})
				added, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"id": itemID, "type": "function_call", "call_id": call.CallID, "name": call.Name, "arguments": "", "status": "in_progress"}})
				delta, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": index, "delta": call.Arguments})
				doneArgs, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": index, "arguments": call.Arguments})
				doneItem, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": output[len(output)-1]})
				_, _ = w.Write([]byte("event: response.output_item.added\ndata: " + string(added) + "\n\n"))
				_, _ = w.Write([]byte("event: response.function_call_arguments.delta\ndata: " + string(delta) + "\n\n"))
				_, _ = w.Write([]byte("event: response.function_call_arguments.done\ndata: " + string(doneArgs) + "\n\n"))
				_, _ = w.Write([]byte("event: response.output_item.done\ndata: " + string(doneItem) + "\n\n"))
			}
			completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": responsesObject(id, spec.Name, "completed", output, responsesUsage(promptTokens, maxInt(len(output)*12, 8), 0))})
			_, _ = w.Write([]byte("event: response.completed\ndata: " + string(completed) + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return false
		}
		messageItem := map[string]any{"id": responseID("msg"), "type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": result.content}}}
		added, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": messageItem})
		doneItem, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": messageItem})
		completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": responsesObject(id, spec.Name, "completed", []map[string]any{messageItem}, responsesUsage(promptTokens, tokens.EstimateText(result.content), tokens.EstimateText(result.reasoning)))})
		_, _ = w.Write([]byte("event: response.output_item.added\ndata: " + string(added) + "\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: " + string(doneItem) + "\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: " + string(completed) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return false
	})
}
