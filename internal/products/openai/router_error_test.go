package openai

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
)

func TestWrappedUpstream429IsRateLimited(t *testing.T) {
	err := fmt.Errorf("bad response status code 429, message: %w", &xai.UpstreamError{
		Status: http.StatusTooManyRequests,
		Body:   `{"error":{"code":8,"message":"Too many requests","details":[]}}`,
	})

	if got := httpStatusForError(err); got != http.StatusTooManyRequests {
		t.Fatalf("httpStatusForError() = %d, want %d", got, http.StatusTooManyRequests)
	}
	if !shouldRetry(err, map[int]struct{}{http.StatusTooManyRequests: {}}, 0, 1) {
		t.Fatalf("shouldRetry() = false, want true")
	}
	if got := feedbackForError(err).Kind; got != account.FeedbackRateLimited {
		t.Fatalf("feedbackForError().Kind = %s, want %s", got, account.FeedbackRateLimited)
	}
}
