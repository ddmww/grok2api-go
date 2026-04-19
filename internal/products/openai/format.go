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

func chatResponse(modelName, content, reasoning string, prompt any) map[string]any {
	promptTokens := tokens.EstimateAny(prompt)
	completionTokens := tokens.EstimateText(content) + tokens.EstimateText(reasoning)
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
		"usage": buildUsage(promptTokens, completionTokens, tokens.EstimateText(reasoning)),
	}
}

func chatToolResponse(modelName string, toolCalls []ParsedToolCall, prompt any) map[string]any {
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
		"usage": buildUsage(tokens.EstimateAny(prompt), maxInt(len(toolCalls)*16, 8), 0),
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
