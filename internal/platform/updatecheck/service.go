package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const defaultAPIBaseURL = "https://api.github.com"

type ReleaseInfo struct {
	Status          string `json:"status"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	HasUpdate       bool   `json:"has_update"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseNotes    string `json:"release_notes"`
	ReleaseURL      string `json:"release_url"`
	PublishedAt     string `json:"published_at"`
}

type Service struct {
	owner      string
	repo       string
	current    string
	apiBaseURL string
	client     *http.Client
	ttl        time.Duration
	now        func() time.Time

	mu        sync.Mutex
	cached    ReleaseInfo
	expiresAt time.Time
}

type release struct {
	TagName     string `json:"tag_name"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
}

func NewService(current, owner, repo string) *Service {
	return &Service{
		owner:      owner,
		repo:       repo,
		current:    strings.TrimSpace(current),
		apiBaseURL: defaultAPIBaseURL,
		client:     &http.Client{Timeout: 5 * time.Second},
		ttl:        10 * time.Minute,
		now:        time.Now,
	}
}

func (s *Service) SetAPIBaseURL(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(value) != "" {
		s.apiBaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
	}
}

func (s *Service) GetLatestReleaseInfo(ctx context.Context, force bool) ReleaseInfo {
	s.mu.Lock()
	if !force && !s.expiresAt.IsZero() && s.now().Before(s.expiresAt) {
		cached := s.cached
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	info, err := s.fetch(ctx)
	if err != nil {
		info = ReleaseInfo{
			Status:          "error",
			CurrentVersion:  s.current,
			LatestVersion:   s.current,
			HasUpdate:       false,
			UpdateAvailable: false,
		}
	}

	s.mu.Lock()
	s.cached = info
	s.expiresAt = s.now().Add(s.ttl)
	s.mu.Unlock()
	return info
}

func (s *Service) fetch(ctx context.Context) (ReleaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=10", s.apiBaseURL, s.owner, s.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ReleaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "grok2api-go-update-check")

	resp, err := s.client.Do(req)
	if err != nil {
		return ReleaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReleaseInfo{}, fmt.Errorf("github releases returned %d", resp.StatusCode)
	}

	var releases []release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return ReleaseInfo{}, err
	}
	selected := selectLatestRelease(releases)
	if selected == nil {
		return ReleaseInfo{}, fmt.Errorf("no valid releases found")
	}

	latest := normalizeComparableVersion(selected.TagName)
	hasUpdate := isNewerVersion(s.current, selected.TagName)
	return ReleaseInfo{
		Status:          "ok",
		CurrentVersion:  s.current,
		LatestVersion:   latest,
		HasUpdate:       hasUpdate,
		UpdateAvailable: hasUpdate,
		ReleaseNotes:    selected.Body,
		ReleaseURL:      strings.TrimSpace(selected.HTMLURL),
		PublishedAt:     strings.TrimSpace(selected.PublishedAt),
	}, nil
}

func selectLatestRelease(releases []release) *release {
	for _, item := range releases {
		if item.Draft || item.Prerelease {
			continue
		}
		if normalizeComparableVersion(item.TagName) == "" {
			continue
		}
		releaseCopy := item
		return &releaseCopy
	}
	return nil
}

func isNewerVersion(current, latest string) bool {
	currentVersion := normalizeComparableVersion(current)
	latestVersion := normalizeComparableVersion(latest)
	if latestVersion == "" {
		return false
	}
	if currentVersion == "" {
		return currentVersion != latestVersion
	}
	return semver.Compare("v"+currentVersion, "v"+latestVersion) < 0
}

func normalizeComparableVersion(value string) string {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(text, "v")
	if text == "" {
		return ""
	}
	if cut := strings.IndexAny(text, "-+"); cut >= 0 {
		text = text[:cut]
	}
	candidate := "v" + text
	if !semver.IsValid(candidate) {
		return ""
	}
	return strings.TrimPrefix(semver.Canonical(candidate), "v")
}
