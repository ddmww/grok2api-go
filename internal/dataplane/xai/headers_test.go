package xai

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ddmww/grok2api-go/internal/testutil"
	"github.com/google/uuid"
)

func TestBuildSSOCookie(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"clearance": map[string]any{
				"cf_cookies": "cf_clearance=clearance-token; __cf_bm=bm-token",
			},
		},
	})

	cookie := buildSSOCookie(cfg, "  sso=abc123 \u200b ", nil)
	if !strings.HasPrefix(cookie, "sso=abc123; sso-rw=abc123") {
		t.Fatalf("unexpected sso cookie: %s", cookie)
	}
	if !strings.Contains(cookie, "cf_clearance=clearance-token") || !strings.Contains(cookie, "__cf_bm=bm-token") {
		t.Fatalf("missing cloudflare cookies: %s", cookie)
	}
}

func TestBuildRequestHeadersChromiumProfile(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"clearance": map[string]any{
				"browser":    "chrome136",
				"user_agent": defaultUserAgent,
				"cf_cookies": "cf_clearance=clearance-token; __cf_bm=bm-token",
			},
		},
	})

	headers := buildRequestHeaders(cfg, "sso=abc123", "application/json", "https://grok.com", "https://grok.com/", nil)
	if got := headers.Get("Baggage"); got != defaultBaggage {
		t.Fatalf("unexpected baggage header: %s", got)
	}
	if got := headers.Get("Sec-Fetch-Site"); got != "same-origin" {
		t.Fatalf("unexpected fetch site: %s", got)
	}
	if got := headers.Get("Sec-Ch-Ua"); !strings.Contains(got, `"Google Chrome";v="136"`) {
		t.Fatalf("unexpected sec-ch-ua: %s", got)
	}
	if got := headers.Get("Sec-Ch-Ua-Platform"); got != `"macOS"` {
		t.Fatalf("unexpected sec-ch-ua-platform: %s", got)
	}
	if got := headers.Get("Cookie"); !strings.Contains(got, "cf_clearance=clearance-token") {
		t.Fatalf("unexpected cookie header: %s", got)
	}
	if got := headers.Get("x-xai-request-id"); got == "" {
		t.Fatal("missing x-xai-request-id")
	} else if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("request id is not a uuid: %v", err)
	}
}

func TestStatsigIDDynamicMode(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"features": map[string]any{
			"dynamic_statsig": true,
		},
	})

	value := statsigID(cfg)
	if value == defaultStatsigID {
		t.Fatal("expected dynamic statsig payload")
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode statsig: %v", err)
	}
	if !strings.HasPrefix(string(decoded), "e:TypeError:") {
		t.Fatalf("unexpected statsig payload: %s", string(decoded))
	}
}
