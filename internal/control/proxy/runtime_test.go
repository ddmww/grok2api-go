package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ddmww/grok2api-go/internal/testutil"
)

func TestRuntimeClearanceManual(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"clearance": map[string]any{
				"mode":       "manual",
				"cf_cookies": "cf_clearance=manual-token; __cf_bm=bm-token",
				"user_agent": "Mozilla/5.0 Chrome/136.0.0.0 Safari/537.36",
				"browser":    "chrome136",
			},
		},
	})
	runtime := NewRuntime(cfg)
	bundle, err := runtime.Clearance("")
	if err != nil {
		t.Fatalf("clearance manual: %v", err)
	}
	if bundle == nil {
		t.Fatal("expected manual bundle")
	}
	if !strings.Contains(bundle.CFCookies, "cf_clearance=manual-token") {
		t.Fatalf("unexpected cookies: %#v", bundle)
	}
}

func TestRuntimeClearanceFlaresolverr(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"ok",
			"solution":{
				"userAgent":"Mozilla/5.0 Chrome/136.0.0.0 Safari/537.36",
				"cookies":[
					{"name":"cf_clearance","value":"fs-token"},
					{"name":"__cf_bm","value":"bm-token"}
				]
			}
		}`))
	}))
	defer server.Close()

	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"clearance": map[string]any{
				"mode":             "flaresolverr",
				"flaresolverr_url": server.URL,
				"timeout_sec":      5,
				"refresh_interval": 60,
			},
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	runtime := NewRuntime(cfg)
	bundle, err := runtime.Clearance("")
	if err != nil {
		t.Fatalf("clearance flaresolverr: %v", err)
	}
	if bundle == nil {
		t.Fatal("expected flaresolverr bundle")
	}
	if bundle.Browser != "chrome136" {
		t.Fatalf("unexpected browser: %#v", bundle)
	}
	if !strings.Contains(bundle.CFCookies, "cf_clearance=fs-token") {
		t.Fatalf("unexpected cookies: %#v", bundle)
	}
}

func TestRuntimeClientCachingAndSessionClients(t *testing.T) {
	cfg := testutil.NewConfig(map[string]any{
		"proxy": map[string]any{
			"egress": map[string]any{
				"mode": "direct",
			},
		},
	})
	runtime := NewRuntime(cfg)

	cachedOne, err := runtime.ClientForProxyURL("")
	if err != nil {
		t.Fatalf("cached client one: %v", err)
	}
	cachedTwo, err := runtime.ClientForProxyURL("")
	if err != nil {
		t.Fatalf("cached client two: %v", err)
	}
	if cachedOne != cachedTwo {
		t.Fatal("expected cached clients to be reused for the same affinity key")
	}

	sessionOne, err := runtime.NewSessionClientForProxyURL("")
	if err != nil {
		t.Fatalf("session client one: %v", err)
	}
	sessionTwo, err := runtime.NewSessionClientForProxyURL("")
	if err != nil {
		t.Fatalf("session client two: %v", err)
	}
	if sessionOne == sessionTwo {
		t.Fatal("expected session-scoped clients to be distinct")
	}
	if transport, ok := sessionOne.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	if transport, ok := sessionTwo.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}
