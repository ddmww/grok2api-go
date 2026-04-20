package updatecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetLatestReleaseInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/ddmww/grok2api-go/releases" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `[
			{"tag_name":"v0.2.0-beta.1","draft":false,"prerelease":true,"body":"beta","html_url":"https://example.com/beta","published_at":"2026-04-19T12:00:00Z"},
			{"tag_name":"v0.2.0","draft":false,"prerelease":false,"body":"stable notes","html_url":"https://example.com/stable","published_at":"2026-04-18T12:00:00Z"}
		]`)
	}))
	defer server.Close()

	service := NewService("0.1.0", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	info := service.GetLatestReleaseInfo(context.Background(), true)

	if info.Status != "ok" {
		t.Fatalf("status mismatch: %#v", info)
	}
	if info.LatestVersion != "0.2.0" {
		t.Fatalf("latest version mismatch: %#v", info)
	}
	if !info.UpdateAvailable || !info.HasUpdate {
		t.Fatalf("expected update available: %#v", info)
	}
	if info.ReleaseURL != "https://example.com/stable" {
		t.Fatalf("release url mismatch: %#v", info)
	}
}

func TestGetLatestReleaseInfoIgnoresPrereleaseWhenCurrentDevMatchesStable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"v0.1.0","draft":false,"prerelease":false,"body":"notes","html_url":"https://example.com/stable","published_at":"2026-04-18T12:00:00Z"}]`)
	}))
	defer server.Close()

	service := NewService("0.1.0-dev", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	info := service.GetLatestReleaseInfo(context.Background(), true)
	if info.UpdateAvailable || info.HasUpdate {
		t.Fatalf("expected no update when base versions match: %#v", info)
	}
}

func TestGetLatestReleaseInfoFallsBackOnNoStableRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"v0.2.0-beta.1","draft":false,"prerelease":true}]`)
	}))
	defer server.Close()

	service := NewService("0.1.0", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	info := service.GetLatestReleaseInfo(context.Background(), true)
	if info.Status != "error" {
		t.Fatalf("expected error fallback: %#v", info)
	}
	if info.LatestVersion != "0.1.0" {
		t.Fatalf("expected latest version to fall back to current: %#v", info)
	}
}

func TestGetLatestReleaseInfoCachesUntilForced(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		fmt.Fprint(w, `[{"tag_name":"v0.3.0","draft":false,"prerelease":false}]`)
	}))
	defer server.Close()

	service := NewService("0.1.0", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	service.ttl = time.Hour

	_ = service.GetLatestReleaseInfo(context.Background(), false)
	_ = service.GetLatestReleaseInfo(context.Background(), false)
	if requests != 1 {
		t.Fatalf("expected one request, got %d", requests)
	}
	_ = service.GetLatestReleaseInfo(context.Background(), true)
	if requests != 2 {
		t.Fatalf("expected forced refresh to bypass cache, got %d", requests)
	}
}
