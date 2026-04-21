package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/model"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/auth"
	"github.com/ddmww/grok2api-go/internal/platform/tokens"
	"github.com/ddmww/grok2api-go/internal/platform/upstreamblocker"
	"github.com/gin-gonic/gin"
)

type messagesRequest struct {
	Model       string           `json:"model"`
	Messages    []map[string]any `json:"messages"`
	System      any              `json:"system"`
	MaxTokens   *int             `json:"max_tokens"`
	Stream      *bool            `json:"stream"`
	Temperature *float64         `json:"temperature"`
	TopP        *float64         `json:"top_p"`
	Tools       []map[string]any `json:"tools"`
	ToolChoice  any              `json:"tool_choice"`
	Thinking    any              `json:"thinking"`
}

type runResult struct {
	content       string
	reasoning     string
	toolCalls     []toolCall
	annotations   []map[string]any
	searchSources []map[string]any
	inputTokens   int
	outputTokens  int
	usage         map[string]any
}

type toolCall struct {
	ID        string
	Name      string
	Arguments string
}

func Mount(router *gin.Engine, state *app.State) {
	v1 := router.Group("/v1")
	v1.Use(auth.APIKey(state.Config))
	v1.POST("/messages", func(c *gin.Context) {
		var request messagesRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
			return
		}
		spec, ok := model.Get(request.Model)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": fmt.Sprintf("Model %q does not exist or you do not have access to it.", request.Model)}})
			return
		}
		if len(request.Messages) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "messages cannot be empty"}})
			return
		}
		stream := state.Config.GetBool("features.stream", true)
		if request.Stream != nil {
			stream = *request.Stream
		}
		if stream {
			streamMessages(c, state, spec, request)
			return
		}
		result, err := runMessages(c.Request.Context(), state, spec, request)
		if err != nil {
			writeAnthropicError(c, err)
			return
		}
		if err := upstreamblocker.AssertResponseAllowed(upstreamblocker.GetConfig(state.Config), result.content, "/v1/messages"); err != nil {
			writeAnthropicError(c, err)
			return
		}
		content := []map[string]any{}
		stopReason := "end_turn"
		if len(result.toolCalls) > 0 {
			stopReason = "tool_use"
			for _, call := range result.toolCalls {
				var parsed map[string]any
				_ = json.Unmarshal([]byte(call.Arguments), &parsed)
				if parsed == nil {
					parsed = map[string]any{}
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": parsed,
				})
			}
		} else {
			if request.Thinking != nil && strings.TrimSpace(result.reasoning) != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": result.reasoning})
			}
			textBlock := map[string]any{"type": "text", "text": result.content}
			if len(result.annotations) > 0 {
				textBlock["annotations"] = result.annotations
			}
			content = append(content, textBlock)
		}
		response := map[string]any{
			"id":            responseID("msg"),
			"type":          "message",
			"role":          "assistant",
			"model":         spec.Name,
			"content":       content,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         anthropicUsage(result),
		}
		if len(result.searchSources) > 0 {
			response["search_sources"] = result.searchSources
		}
		c.JSON(http.StatusOK, response)
	})
}

func streamMessages(c *gin.Context, state *app.State, spec model.Spec, request messagesRequest) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	result, err := runMessages(c.Request.Context(), state, spec, request)
	if err != nil {
		payload := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}}
		_ = writeEvent(c, "error", payload)
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
		return
	}

	messageID := responseID("msg")
	_ = writeEvent(c, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      messageID,
			"type":    "message",
			"role":    "assistant",
			"model":   spec.Name,
			"content": []map[string]any{},
			"usage":   map[string]any{"input_tokens": anthropicInputTokens(result), "output_tokens": 0},
		},
	})
	blockIndex := 0
	if request.Thinking != nil && strings.TrimSpace(result.reasoning) != "" && len(result.toolCalls) == 0 {
		_ = writeEvent(c, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
		_ = writeEvent(c, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": result.reasoning},
		})
		_ = writeEvent(c, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		blockIndex++
	}
	if len(result.toolCalls) > 0 {
		for _, call := range result.toolCalls {
			_ = writeEvent(c, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": map[string]any{},
				},
			})
			_ = writeEvent(c, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
			})
			_ = writeEvent(c, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			blockIndex++
		}
		msgDelta := map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}
		if len(result.searchSources) > 0 {
			msgDelta["search_sources"] = result.searchSources
		}
		_ = writeEvent(c, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": msgDelta,
			"usage": map[string]any{"output_tokens": anthropicOutputTokens(result)},
		})
	} else {
		_ = writeEvent(c, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		delta := map[string]any{"type": "text_delta", "text": result.content}
		if len(result.annotations) > 0 {
			delta["annotations"] = result.annotations
		}
		_ = writeEvent(c, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": delta,
		})
		_ = writeEvent(c, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		msgDelta := map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}
		if len(result.searchSources) > 0 {
			msgDelta["search_sources"] = result.searchSources
		}
		_ = writeEvent(c, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": msgDelta,
			"usage": map[string]any{"output_tokens": anthropicOutputTokens(result)},
		})
	}
	_ = writeEvent(c, "message_stop", map[string]any{"type": "message_stop"})
}

func runMessages(ctx context.Context, state *app.State, spec model.Spec, request messagesRequest) (runResult, error) {
	messages := parseAnthropicMessages(request.Messages, request.System)
	message := flattenMessages(messages)
	toolNames := []string{}
	if len(request.Tools) > 0 {
		tools := convertTools(request.Tools)
		message = injectAnthropicToolPrompt(message, buildAnthropicToolPrompt(tools, request.ToolChoice))
		toolNames = extractAnthropicToolNames(tools)
	}
	retryCodes := parseRetryCodes(state.Config.GetString("retry.on_codes", "429,503"))
	maxRetries := maxInt(state.Config.GetInt("retry.max_retries", 1), 0)
	excluded := map[string]struct{}{}
	inputTokens := tokens.EstimateTextByModel(spec.Name, message)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		lease, err := state.Runtime.ReserveWithExclude(spec, excluded)
		if err != nil {
			return runResult{}, err
		}
		lines, errCh := state.XAI.ChatStream(ctx, lease.Token, buildReversePayload(state, spec, message))
		result := runResult{inputTokens: inputTokens}
		adapter := xai.NewStreamAdapter(state.Config)
		for line := range lines {
			kind, data := xai.ClassifyLine(line)
			if kind != "data" {
				continue
			}
			stop := false
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
				case "soft_stop":
					stop = true
				}
			}
			if stop {
				break
			}
		}
		if err := <-errCh; err != nil {
			_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForAnthropic(err))
			if shouldRetryAnthropic(err, retryCodes, attempt, maxRetries) {
				excluded[lease.Token] = struct{}{}
				continue
			}
			return runResult{}, err
		}
		if len(toolNames) > 0 {
			result.toolCalls = parseAnthropicToolCalls(result.content, toolNames)
			if len(result.toolCalls) > 0 {
				result.content = ""
				result.outputTokens = len(result.toolCalls) * 8
			}
		}
		result.searchSources = adapter.SearchSourcesList()
		if result.outputTokens == 0 {
			result.outputTokens = tokens.EstimateTextByModel(spec.Name, result.content) + tokens.EstimateTextByModel(spec.Name, result.reasoning)
		}
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
		return result, nil
	}
	return runResult{}, fmt.Errorf("no available accounts for this model tier")
}

func anthropicUsage(result runResult) map[string]any {
	return map[string]any{
		"input_tokens":  anthropicInputTokens(result),
		"output_tokens": anthropicOutputTokens(result),
	}
}

func anthropicInputTokens(result runResult) int {
	if len(result.usage) > 0 {
		if value := anthropicUsageInt(result.usage["input_tokens"]); value > 0 {
			return value
		}
		if value := anthropicUsageInt(result.usage["prompt_tokens"]); value > 0 {
			return value
		}
	}
	return result.inputTokens
}

func anthropicOutputTokens(result runResult) int {
	if len(result.usage) > 0 {
		if value := anthropicUsageInt(result.usage["output_tokens"]); value > 0 {
			return value
		}
		if value := anthropicUsageInt(result.usage["completion_tokens"]); value > 0 {
			return value
		}
	}
	return result.outputTokens
}

func anthropicUsageInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func parseAnthropicMessages(messages []map[string]any, system any) []map[string]any {
	internal := make([]map[string]any, 0, len(messages)+1)
	if system != nil {
		switch typed := system.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				internal = append(internal, map[string]any{"role": "system", "content": typed})
			}
		case []any:
			parts := []string{}
			for _, item := range typed {
				mapped, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if text, _ := mapped["text"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
			if len(parts) > 0 {
				internal = append(internal, map[string]any{"role": "system", "content": strings.Join(parts, "\n")})
			}
		}
	}
	for _, message := range messages {
		role, _ := message["role"].(string)
		internal = append(internal, normalizeAnthropicContent(role, message["content"])...)
	}
	return internal
}

func normalizeAnthropicContent(role string, content any) []map[string]any {
	switch typed := content.(type) {
	case string:
		return []map[string]any{{"role": role, "content": typed}}
	case []any:
		textParts := []string{}
		toolCalls := []map[string]any{}
		toolResults := []map[string]any{}
		blocks := []map[string]any{}
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
					textParts = append(textParts, strings.TrimSpace(text))
					blocks = append(blocks, map[string]any{"type": "text", "text": strings.TrimSpace(text)})
				}
			case "image":
				if normalized := anthropicImageBlock(block); normalized != nil {
					blocks = append(blocks, normalized)
				}
			case "document":
				if normalized := anthropicDocumentBlock(block); normalized != nil {
					blocks = append(blocks, normalized)
				}
			case "tool_use":
				inputJSON, _ := json.Marshal(block["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   defaultString(block["id"], responseID("toolu")),
					"type": "function",
					"function": map[string]any{
						"name":      defaultString(block["name"], ""),
						"arguments": string(inputJSON),
					},
				})
			case "tool_result":
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": defaultString(block["tool_use_id"], ""),
					"content":      anthropicToolResultText(block["content"]),
				})
			}
		}
		if len(toolResults) > 0 {
			return toolResults
		}
		if len(toolCalls) > 0 {
			return []map[string]any{{
				"role":       "assistant",
				"content":    strings.Join(textParts, " "),
				"tool_calls": toolCalls,
			}}
		}
		if len(blocks) > 0 {
			return []map[string]any{{"role": role, "content": blocks}}
		}
	}
	return nil
}

func anthropicImageBlock(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	sourceType, _ := source["type"].(string)
	switch sourceType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if data == "" {
			return nil
		}
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data}}
	case "url":
		if raw, _ := source["url"].(string); raw != "" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": raw}}
		}
	}
	return nil
}

func anthropicDocumentBlock(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	sourceType, _ := source["type"].(string)
	if sourceType != "base64" {
		return nil
	}
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	if data == "" {
		return nil
	}
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	return map[string]any{"type": "file", "file": map[string]any{"data": "data:" + mediaType + ";base64," + data}}
}

func anthropicToolResultText(content any) string {
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
			if text, _ := mapped["text"].(string); strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func convertTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        defaultString(tool["name"], ""),
				"description": defaultString(tool["description"], ""),
				"parameters":  tool["input_schema"],
			},
		})
	}
	return out
}

func injectAnthropicToolPrompt(message, systemPrompt string) string {
	return "[system]: " + systemPrompt + "\n\n" + message
}

func buildAnthropicToolPrompt(tools []map[string]any, toolChoice any) string {
	toolDefs := make([]string, 0, len(tools))
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		description, _ := function["description"].(string)
		line := fmt.Sprintf("Tool: %s", name)
		if description != "" {
			line += "\nDescription: " + description
		}
		if params := function["parameters"]; params != nil {
			if payload, err := json.Marshal(params); err == nil {
				line += "\nParameters: " + string(payload)
			}
		}
		toolDefs = append(toolDefs, line)
	}
	choice := "WHEN TO CALL: Call a tool when it is clearly needed. Otherwise respond in plain text."
	switch typed := convertAnthropicToolChoice(toolChoice).(type) {
	case string:
		switch typed {
		case "none":
			choice = "WHEN TO CALL: Do NOT call any tools. Respond in plain text only."
		case "required":
			choice = "WHEN TO CALL: You MUST output a <tool_calls> XML block. Do NOT write any plain-text reply under any circumstances."
		}
	case map[string]any:
		function, _ := typed["function"].(map[string]any)
		if name, _ := function["name"].(string); name != "" {
			choice = fmt.Sprintf("WHEN TO CALL: You MUST output a <tool_calls> XML block calling the tool named %q.", name)
		}
	}
	return fmt.Sprintf(`You have access to the following tools.

AVAILABLE TOOLS:
%s

TOOL CALL FORMAT — follow these rules exactly:
- When calling a tool, output ONLY the XML block below. No text before or after it.
- <parameters> must be a single-line valid JSON object (no line breaks inside).
- Place multiple tool calls inside ONE <tool_calls> element.
- Do NOT use markdown code fences around the XML.

<tool_calls>
  <tool_call>
    <tool_name>TOOL_NAME</tool_name>
    <parameters>{"key":"value"}</parameters>
  </tool_call>
</tool_calls>

%s`, strings.Join(toolDefs, "\n\n"), choice)
}

func extractAnthropicToolNames(tools []map[string]any) []string {
	out := []string{}
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) != "" {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

func convertAnthropicToolChoice(choice any) any {
	switch typed := choice.(type) {
	case nil:
		return "auto"
	case string:
		return typed
	case map[string]any:
		switch typed["type"] {
		case "any":
			return "required"
		case "tool":
			return map[string]any{"type": "function", "function": map[string]any{"name": defaultString(typed["name"], "")}}
		default:
			return "auto"
		}
	default:
		return "auto"
	}
}

func parseAnthropicToolCalls(text string, available []string) []toolCall {
	parsed := parseToolCallsLocal(text, available)
	out := make([]toolCall, 0, len(parsed))
	for _, item := range parsed {
		out = append(out, toolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
	}
	return out
}

func defaultString(value any, fallback string) string {
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	return fallback
}

type parsedToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

var (
	xmlRootRe   = regexp.MustCompile(`(?is)<tool_calls\s*>(.*?)</tool_calls\s*>`)
	xmlCallRe   = regexp.MustCompile(`(?is)<tool_call\s*>(.*?)</tool_call\s*>`)
	xmlNameRe   = regexp.MustCompile(`(?is)<tool_name\s*>(.*?)</tool_name\s*>`)
	xmlParamRe  = regexp.MustCompile(`(?is)<parameters\s*>(.*?)</parameters\s*>`)
	jsonObjRe   = regexp.MustCompile(`(?s)\{.*\}`)
	jsonArrayRe = regexp.MustCompile(`(?s)\[.*\]`)
)

func parseToolCallsLocal(text string, available []string) []parsedToolCall {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	root := xmlRootRe.FindStringSubmatch(text)
	if len(root) > 1 {
		calls := []parsedToolCall{}
		for _, match := range xmlCallRe.FindAllStringSubmatch(root[1], -1) {
			nameMatch := xmlNameRe.FindStringSubmatch(match[1])
			if len(nameMatch) < 2 {
				continue
			}
			name := strings.TrimSpace(nameMatch[1])
			if len(available) > 0 && !containsToolName(available, name) {
				continue
			}
			parameters := "{}"
			if params := xmlParamRe.FindStringSubmatch(match[1]); len(params) > 1 {
				if normalized := normalizeJSONLocal(strings.TrimSpace(params[1])); normalized != "" {
					parameters = normalized
				}
			}
			calls = append(calls, parsedToolCall{
				CallID:    fmt.Sprintf("call_%d%s", time.Now().UnixMilli(), localRandomSuffix()),
				Name:      name,
				Arguments: parameters,
			})
		}
		return calls
	}
	if objMatch := jsonObjRe.FindString(text); objMatch != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(objMatch), &obj); err == nil {
			if toolCalls, ok := obj["tool_calls"].([]any); ok {
				return parseJSONToolCallsLocal(toolCalls, available)
			}
		}
	}
	if arrMatch := jsonArrayRe.FindString(text); arrMatch != "" {
		var arr []any
		if err := json.Unmarshal([]byte(arrMatch), &arr); err == nil {
			return parseJSONToolCallsLocal(arr, available)
		}
	}
	return nil
}

func parseJSONToolCallsLocal(items []any, available []string) []parsedToolCall {
	out := []parsedToolCall{}
	for _, item := range items {
		mapped, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := mapped["name"].(string)
		if name == "" {
			name, _ = mapped["tool_name"].(string)
		}
		if name == "" || (len(available) > 0 && !containsToolName(available, name)) {
			continue
		}
		args := "{}"
		if value := mapped["input"]; value != nil {
			if data, err := json.Marshal(value); err == nil {
				args = string(data)
			}
		} else if value := mapped["arguments"]; value != nil {
			if typed, ok := value.(string); ok && normalizeJSONLocal(typed) != "" {
				args = normalizeJSONLocal(typed)
			}
		}
		out = append(out, parsedToolCall{
			CallID:    fmt.Sprintf("call_%d%s", time.Now().UnixMilli(), localRandomSuffix()),
			Name:      name,
			Arguments: args,
		})
	}
	return out
}

func normalizeJSONLocal(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func containsToolName(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func localRandomSuffix() string {
	return fmt.Sprintf("%x", os.Getpid())[:3]
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
	if response["finalMetadata"] != nil {
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
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		content := stringifyContent(msg["content"])
		if strings.TrimSpace(content) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s]: %s", role, content))
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
			if text, _ := mapped["text"].(string); text != "" {
				parts = append(parts, text)
				continue
			}
			if imageURL, ok := mapped["image_url"].(map[string]any); ok {
				if raw, _ := imageURL["url"].(string); raw != "" {
					parts = append(parts, raw)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func buildReversePayload(state *app.State, spec model.Spec, message string) map[string]any {
	return map[string]any{
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
	}
}

func shouldRetryAnthropic(err error, retryCodes map[int]struct{}, attempt, maxRetries int) bool {
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

func feedbackForAnthropic(err error) account.Feedback {
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

func parseRetryCodes(raw string) map[int]struct{} {
	out := map[int]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		var code int
		if _, err := fmt.Sscanf(strings.TrimSpace(part), "%d", &code); err == nil {
			out[code] = struct{}{}
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func responseID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func writeAnthropicError(c *gin.Context, err error) {
	errorType := "api_error"
	if _, ok := err.(*upstreamblocker.Error); ok {
		errorType = "upstream_blocked"
	}
	c.JSON(httpStatusForError(err), gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errorType,
			"message": err.Error(),
		},
	})
}

func httpStatusForError(err error) int {
	if _, ok := err.(*upstreamblocker.Error); ok {
		return http.StatusForbidden
	}
	if upstream, ok := err.(*xai.UpstreamError); ok {
		if upstream.Status > 0 {
			return upstream.Status
		}
	}
	return http.StatusInternalServerError
}

func writeEvent(c *gin.Context, event string, payload any) bool {
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
