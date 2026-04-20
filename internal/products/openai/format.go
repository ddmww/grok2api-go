package openai

import (
	"fmt"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/tokens"
)

func responseID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixMilli())
}

func buildUsage(promptTokens, completionTokens, reasoningTokens int) map[string]any {
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0,
			"text_tokens":   promptTokens,
			"audio_tokens":  0,
			"image_tokens":  0,
		},
		"completion_tokens_details": map[string]any{
			"text_tokens":      maxInt(completionTokens-reasoningTokens, 0),
			"audio_tokens":     0,
			"reasoning_tokens": reasoningTokens,
		},
	}
}

func chatUsageOrEstimate(override map[string]any, prompt any, content, reasoning string) map[string]any {
	if usage := normalizeChatUsage(override); usage != nil {
		return usage
	}
	promptTokens := tokens.EstimateAny(prompt)
	completionTokens := tokens.EstimateText(content) + tokens.EstimateText(reasoning)
	return buildUsage(promptTokens, completionTokens, tokens.EstimateText(reasoning))
}

func chatToolUsageOrEstimate(override map[string]any, prompt any, toolCalls []ParsedToolCall) map[string]any {
	if usage := normalizeChatUsage(override); usage != nil {
		return usage
	}
	return buildUsage(tokens.EstimateAny(prompt), maxInt(len(toolCalls)*16, 8), 0)
}

func chatResponse(modelName, content, reasoning string, prompt any, usage map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	return map[string]any{
		"id":      responseID("chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": chatUsageOrEstimate(usage, prompt, content, reasoning),
	}
}

func chatToolResponse(modelName string, toolCalls []ParsedToolCall, prompt any, usage map[string]any) map[string]any {
	out := make([]map[string]any, 0, len(toolCalls))
	for _, call := range toolCalls {
		out = append(out, map[string]any{
			"id":   call.CallID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return map[string]any{
		"id":      responseID("chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": out,
			},
			"finish_reason": "tool_calls",
		}},
		"usage": chatToolUsageOrEstimate(usage, prompt, toolCalls),
	}
}

func responsesObject(id, modelName, status string, output []map[string]any, usage map[string]any) map[string]any {
	body := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      modelName,
		"output":     output,
	}
	if usage != nil {
		body["usage"] = usage
	}
	return body
}

func responsesUsage(promptTokens, outputTokens, reasoningTokens int) map[string]any {
	return map[string]any{
		"input_tokens":  promptTokens,
		"output_tokens": outputTokens,
		"total_tokens":  promptTokens + outputTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoningTokens,
		},
	}
}

func responsesUsageOrEstimate(override map[string]any, prompt any, content, reasoning string) map[string]any {
	if usage := normalizeResponsesUsage(override); usage != nil {
		return usage
	}
	promptTokens := tokens.EstimateAny(prompt)
	outputTokens := tokens.EstimateText(content) + tokens.EstimateText(reasoning)
	return responsesUsage(promptTokens, outputTokens, tokens.EstimateText(reasoning))
}

func responsesToolUsageOrEstimate(override map[string]any, prompt any, toolCalls int) map[string]any {
	if usage := normalizeResponsesUsage(override); usage != nil {
		return usage
	}
	return responsesUsage(tokens.EstimateAny(prompt), maxInt(toolCalls*12, 8), 0)
}

func normalizeChatUsage(override map[string]any) map[string]any {
	if len(override) == 0 {
		return nil
	}
	promptTokens := usageInt(override["prompt_tokens"])
	completionTokens := usageInt(override["completion_tokens"])
	reasoningTokens := usageInt(usageNested(override["completion_tokens_details"], "reasoning_tokens"))
	if promptTokens == 0 && completionTokens == 0 {
		promptTokens = usageInt(override["input_tokens"])
		completionTokens = usageInt(override["output_tokens"])
		reasoningTokens = usageInt(usageNested(override["output_tokens_details"], "reasoning_tokens"))
	}
	if promptTokens == 0 && completionTokens == 0 && usageInt(override["total_tokens"]) == 0 {
		return nil
	}
	return buildUsage(promptTokens, completionTokens, reasoningTokens)
}

func normalizeResponsesUsage(override map[string]any) map[string]any {
	if len(override) == 0 {
		return nil
	}
	inputTokens := usageInt(override["input_tokens"])
	outputTokens := usageInt(override["output_tokens"])
	reasoningTokens := usageInt(usageNested(override["output_tokens_details"], "reasoning_tokens"))
	if inputTokens == 0 && outputTokens == 0 {
		inputTokens = usageInt(override["prompt_tokens"])
		outputTokens = usageInt(override["completion_tokens"])
		reasoningTokens = usageInt(usageNested(override["completion_tokens_details"], "reasoning_tokens"))
	}
	if inputTokens == 0 && outputTokens == 0 && usageInt(override["total_tokens"]) == 0 {
		return nil
	}
	return responsesUsage(inputTokens, outputTokens, reasoningTokens)
}

func usageNested(value any, key string) any {
	mapped, _ := value.(map[string]any)
	if mapped == nil {
		return nil
	}
	return mapped[key]
}

func usageInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
