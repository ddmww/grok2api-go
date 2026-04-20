package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultAPIBaseURL = "https://hub.docker.com/v2/namespaces"

type ReleaseInfo struct {
	Status          string `json:"status"`
	CurrentVersion  string `json:"current_version"`
	CurrentCommit   string `json:"current_commit,omitempty"`
	CurrentImageTag string `json:"current_image_tag,omitempty"`
	LatestVersion   string `json:"latest_version"`
	LatestCommit    string `json:"latest_commit,omitempty"`
	LatestImageTag  string `json:"latest_image_tag,omitempty"`
	HasUpdate       bool   `json:"has_update"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseNotes    string `json:"release_notes"`
	ReleaseURL      string `json:"release_url"`
	PublishedAt     string `json:"published_at"`
}

type Service struct {
	namespace   string
	repo        string
	current     string
	currentSHA  string
	currentTag  string
	apiBaseURL  string
	client      *http.Client
	ttl         time.Duration
	now         func() time.Time
	mu          sync.Mutex
	cached      ReleaseInfo
	expiresAt   time.Time
}

type tagListResponse struct {
	Results []tagDetail `json:"results"`
}

type tagDetail struct {
	Name        string      `json:"name"`
	LastUpdated string      `json:"last_updated"`
	Images      []tagImage  `json:"images"`
	FullSize    int64       `json:"full_size"`
}

type tagImage struct {
	Digest       string `json:"digest"`
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

func NewService(currentVersion, currentCommit, currentImageTag, namespace, repo string) *Service {
	return &Service{
		namespace:  strings.TrimSpace(namespace),
		repo:       strings.TrimSpace(repo),
		current:    strings.TrimSpace(currentVersion),
		currentSHA: strings.TrimSpace(currentCommit),
		currentTag: strings.TrimSpace(currentImageTag),
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
			CurrentCommit:   s.currentSHA,
			CurrentImageTag: s.currentTag,
			LatestVersion:   s.current,
			LatestCommit:    s.currentSHA,
			LatestImageTag:  s.currentTag,
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
	latestTag, err := s.fetchTag(ctx, "latest")
	if err != nil {
		return ReleaseInfo{}, err
	}
	allTags, err := s.fetchTags(ctx)
	if err != nil {
		return ReleaseInfo{}, err
	}

	latestDigest := firstDigest(latestTag.Images)
	latestCommit := findMatchingCommitTag(allTags, latestDigest)
	latestVersion := strings.TrimSpace(latestTag.Name)
	if latestCommit != "" {
		latestVersion = latestCommit
	}

	hasUpdate := false
	currentCommit := strings.TrimSpace(s.currentSHA)
	if currentCommit != "" && latestCommit != "" {
		hasUpdate = !strings.EqualFold(currentCommit, latestCommit)
	} else if strings.TrimSpace(s.currentTag) != "" {
		hasUpdate = !strings.EqualFold(strings.TrimSpace(s.currentTag), strings.TrimSpace(latestTag.Name))
	}

	return ReleaseInfo{
		Status:          "ok",
		CurrentVersion:  s.current,
		CurrentCommit:   currentCommit,
		CurrentImageTag: s.currentTag,
		LatestVersion:   latestVersion,
		LatestCommit:    latestCommit,
		LatestImageTag:  strings.TrimSpace(latestTag.Name),
		HasUpdate:       hasUpdate,
		UpdateAvailable: hasUpdate,
		ReleaseNotes:    buildReleaseNotes(s.namespace, s.repo, latestTag, latestCommit, latestDigest),
		ReleaseURL:      fmt.Sprintf("https://hub.docker.com/r/%s/%s/tags", s.namespace, s.repo),
		PublishedAt:     strings.TrimSpace(latestTag.LastUpdated),
	}, nil
}

func (s *Service) fetchTag(ctx context.Context, tag string) (tagDetail, error) {
	url := fmt.Sprintf("%s/%s/repositories/%s/tags/%s", s.apiBaseURL, s.namespace, s.repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return tagDetail{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok2api-go-update-check")

	resp, err := s.client.Do(req)
	if err != nil {
		return tagDetail{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tagDetail{}, fmt.Errorf("docker hub tag returned %d", resp.StatusCode)
	}

	var detail tagDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return tagDetail{}, err
	}
	return detail, nil
}

func (s *Service) fetchTags(ctx context.Context) ([]tagDetail, error) {
	url := fmt.Sprintf("%s/%s/repositories/%s/tags?page_size=100", s.apiBaseURL, s.namespace, s.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok2api-go-update-check")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker hub tags returned %d", resp.StatusCode)
	}

	var payload tagListResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Results, nil
}

func findMatchingCommitTag(tags []tagDetail, digest string) string {
	if strings.TrimSpace(digest) == "" {
		return ""
	}
	for _, item := range tags {
		if !strings.HasPrefix(strings.TrimSpace(item.Name), "sha-") {
			continue
		}
		if firstDigest(item.Images) == digest {
			return strings.TrimPrefix(strings.TrimSpace(item.Name), "sha-")
		}
	}
	return ""
}

func firstDigest(images []tagImage) string {
	for _, image := range images {
		if strings.TrimSpace(image.Digest) != "" {
			return strings.TrimSpace(image.Digest)
		}
	}
	return ""
}

func buildReleaseNotes(namespace, repo string, latest tagDetail, latestCommit, latestDigest string) string {
	lines := []string{
		fmt.Sprintf("Docker Hub image: `%s/%s:%s`", namespace, repo, strings.TrimSpace(latest.Name)),
	}
	if latestCommit != "" {
		lines = append(lines, fmt.Sprintf("Commit: `%s`", latestCommit))
	}
	if latestDigest != "" {
		lines = append(lines, fmt.Sprintf("Digest: `%s`", latestDigest))
	}
	if latest.LastUpdated != "" {
		lines = append(lines, fmt.Sprintf("Published: `%s`", latest.LastUpdated))
	}
	return strings.Join(lines, "\n")
}
