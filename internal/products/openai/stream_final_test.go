package openai

import (
	"encoding/json"
	"testing"

	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
)

func TestFinalTextCompletionDelta(t *testing.T) {
	adapter := xai.NewStreamAdapter(nil)
	feedFrame(t, adapter, map[string]any{
		"token":      "Hello",
		"messageTag": "final",
	})
	feedFrame(t, adapter, map[string]any{
		"modelResponse": map[string]any{"message": "Hello world"},
	})

	result := streamResult{content: "Hello"}
	if delta := finalTextCompletionDelta(&result, adapter); delta != " world" {
		t.Fatalf("delta mismatch: %q", delta)
	}
	if result.content != "Hello world" {
		t.Fatalf("content mismatch: %q", result.content)
	}
}

func TestFinalTextCompletionDeltaPreservesStreamedLeadingWhitespace(t *testing.T) {
	adapter := xai.NewStreamAdapter(nil)
	feedFrame(t, adapter, map[string]any{
		"modelResponse": map[string]any{"message": "PiuPiu text continues"},
	})

	result := streamResult{content: "\n\nPiuPiu text"}
	if delta := finalTextCompletionDelta(&result, adapter); delta != " continues" {
		t.Fatalf("delta mismatch: %q", delta)
	}
	if result.content != "\n\nPiuPiu text continues" {
		t.Fatalf("content mismatch: %q", result.content)
	}
}

func TestFinalTextCompletionDeltaSkipsDivergentFinalText(t *testing.T) {
	adapter := xai.NewStreamAdapter(nil)
	feedFrame(t, adapter, map[string]any{
		"modelResponse": map[string]any{"message": "Different final"},
	})

	result := streamResult{content: "streamed text"}
	if delta := finalTextCompletionDelta(&result, adapter); delta != "" {
		t.Fatalf("expected no delta, got %q", delta)
	}
	if result.content != "streamed text" {
		t.Fatalf("content should not change: %q", result.content)
	}
}

func feedFrame(t *testing.T, adapter *xai.StreamAdapter, response map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"result": map[string]any{"response": response},
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	adapter.Feed(string(payload))
}
