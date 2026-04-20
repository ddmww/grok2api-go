package updatecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetLatestReleaseInfoFromDockerHub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddmww/repositories/grok2api-go/tags/latest":
			fmt.Fprint(w, `{"name":"latest","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]}`)
		case "/ddmww/repositories/grok2api-go/tags":
			fmt.Fprint(w, `{"results":[
				{"name":"latest","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]},
				{"name":"sha-7518d32","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]},
				{"name":"v0.1.2","last_updated":"2026-04-20T11:00:00Z","images":[{"digest":"sha256:def"}]}
			]}`)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewService("0.1.2", "cde1708", "v0.1.2", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	info := service.GetLatestReleaseInfo(context.Background(), true)

	if info.Status != "ok" {
		t.Fatalf("status mismatch: %#v", info)
	}
	if info.LatestVersion != "7518d32" {
		t.Fatalf("latest version mismatch: %#v", info)
	}
	if info.LatestCommit != "7518d32" {
		t.Fatalf("latest commit mismatch: %#v", info)
	}
	if !info.UpdateAvailable || !info.HasUpdate {
		t.Fatalf("expected update available: %#v", info)
	}
	if info.ReleaseURL != "https://hub.docker.com/r/ddmww/grok2api-go/tags" {
		t.Fatalf("release url mismatch: %#v", info)
	}
}

func TestGetLatestReleaseInfoNoUpdateWhenCommitMatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddmww/repositories/grok2api-go/tags/latest":
			fmt.Fprint(w, `{"name":"latest","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]}`)
		case "/ddmww/repositories/grok2api-go/tags":
			fmt.Fprint(w, `{"results":[
				{"name":"latest","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]},
				{"name":"sha-cde1708","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]}
			]}`)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewService("0.1.2", "cde1708", "v0.1.2", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	info := service.GetLatestReleaseInfo(context.Background(), true)
	if info.UpdateAvailable || info.HasUpdate {
		t.Fatalf("expected no update when commit matches: %#v", info)
	}
}

func TestGetLatestReleaseInfoCachesUntilForced(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/ddmww/repositories/grok2api-go/tags/latest":
			fmt.Fprint(w, `{"name":"latest","last_updated":"2026-04-20T12:00:00Z","images":[{"digest":"sha256:abc"}]}`)
		case "/ddmww/repositories/grok2api-go/tags":
			fmt.Fprint(w, `{"results":[{"name":"latest","images":[{"digest":"sha256:abc"}]},{"name":"sha-7518d32","images":[{"digest":"sha256:abc"}]}]}`)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewService("0.1.2", "cde1708", "v0.1.2", "ddmww", "grok2api-go")
	service.SetAPIBaseURL(server.URL)
	service.ttl = time.Hour

	_ = service.GetLatestReleaseInfo(context.Background(), false)
	_ = service.GetLatestReleaseInfo(context.Background(), false)
	if requests != 2 {
		t.Fatalf("expected one fetch cycle, got %d requests", requests)
	}
	_ = service.GetLatestReleaseInfo(context.Background(), true)
	if requests != 4 {
		t.Fatalf("expected forced refresh to bypass cache, got %d requests", requests)
	}
}
