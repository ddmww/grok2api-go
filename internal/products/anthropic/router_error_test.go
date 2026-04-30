package anthropic

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
	if !shouldRetryAnthropic(err, map[int]struct{}{http.StatusTooManyRequests: {}}, 0, 1) {
		t.Fatalf("shouldRetryAnthropic() = false, want true")
	}
	if got := feedbackForAnthropic(err).Kind; got != account.FeedbackRateLimited {
		t.Fatalf("feedbackForAnthropic().Kind = %s, want %s", got, account.FeedbackRateLimited)
	}
}
