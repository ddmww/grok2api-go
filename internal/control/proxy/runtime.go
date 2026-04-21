package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/config"
)

var chromeVersionPattern = regexp.MustCompile(`Chrome/(\d+)`)

type ClearanceBundle struct {
	CFCookies   string
	UserAgent   string
	Browser     string
	RefreshedAt int64
}

type Runtime struct {
	cfg        *config.Service
	mu         sync.Mutex
	index      int
	cache      map[string]*http.Client
	bundles    map[string]ClearanceBundle
	refreshing map[string]chan struct{}
}

func NewRuntime(cfg *config.Service) *Runtime {
	return &Runtime{
		cfg:        cfg,
		cache:      map[string]*http.Client{},
		bundles:    map[string]ClearanceBundle{},
		refreshing: map[string]chan struct{}{},
	}
}

func (r *Runtime) pick(resource bool) string {
	mode := r.cfg.GetString("proxy.egress.mode", "direct")
	if mode == "single_proxy" {
		if resource {
			if value := r.cfg.GetString("proxy.egress.resource_proxy_url", ""); value != "" {
				return value
			}
		}
		return r.cfg.GetString("proxy.egress.proxy_url", "")
	}
	if mode != "proxy_pool" {
		return ""
	}
	pool := r.cfg.GetStringSlice("proxy.egress.proxy_pool")
	if resource {
		if resourcePool := r.cfg.GetStringSlice("proxy.egress.resource_proxy_pool"); len(resourcePool) > 0 {
			pool = resourcePool
		}
	}
	if len(pool) == 0 {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value := pool[r.index%len(pool)]
	r.index++
	return value
}

func (r *Runtime) Client(resource bool) (*http.Client, string, error) {
	proxyURL := strings.TrimSpace(r.pick(resource))
	key := affinityKey(proxyURL)
	r.mu.Lock()
	if client, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return client, proxyURL, nil
	}
	r.mu.Unlock()

	transport, err := newTransport(r.cfg, proxyURL)
	if err != nil {
		return nil, "", err
	}
	client := &http.Client{Transport: transport}
	r.mu.Lock()
	r.cache[key] = client
	r.mu.Unlock()
	return client, proxyURL, nil
}

func (r *Runtime) ProxyURL(resource bool) string {
	return strings.TrimSpace(r.pick(resource))
}

func (r *Runtime) Clearance(proxyURL string) (*ClearanceBundle, error) {
	switch strings.TrimSpace(r.cfg.GetString("proxy.clearance.mode", "none")) {
	case "none":
		return nil, nil
	case "manual":
		bundle := r.manualBundle()
		if bundle == nil {
			return nil, nil
		}
		return bundle, nil
	case "flaresolverr":
		return r.getOrBuildFlaresolverrBundle(proxyURL)
	default:
		return nil, nil
	}
}

func (r *Runtime) WarmUp() {
	if strings.TrimSpace(r.cfg.GetString("proxy.clearance.mode", "none")) == "none" {
		return
	}
	items := r.affinityItems()
	var wg sync.WaitGroup
	for _, proxyURL := range items {
		wg.Add(1)
		go func(proxyURL string) {
			defer wg.Done()
			_, _ = r.Clearance(proxyURL)
		}(proxyURL)
	}
	wg.Wait()
}

func (r *Runtime) RefreshClearanceSafe() {
	mode := strings.TrimSpace(r.cfg.GetString("proxy.clearance.mode", "none"))
	switch mode {
	case "none":
		return
	case "manual":
		bundle := r.manualBundle()
		if bundle == nil {
			return
		}
		r.mu.Lock()
		for _, proxyURL := range r.affinityItems() {
			r.bundles[affinityKey(proxyURL)] = *bundle
		}
		r.mu.Unlock()
	case "flaresolverr":
		r.refreshFlaresolverrBundles()
	}
}

func (r *Runtime) Reset(proxyURL string) {
	key := affinityKey(proxyURL)

	r.mu.Lock()
	client := r.cache[key]
	delete(r.cache, key)
	r.mu.Unlock()

	if client != nil {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

func (r *Runtime) ResetAll() {
	r.mu.Lock()
	clients := make([]*http.Client, 0, len(r.cache))
	for key, client := range r.cache {
		clients = append(clients, client)
		delete(r.cache, key)
	}
	r.mu.Unlock()

	for _, client := range clients {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

func (r *Runtime) manualBundle() *ClearanceBundle {
	cookies := strings.TrimSpace(r.cfg.GetString("proxy.clearance.cf_cookies", ""))
	userAgent := strings.TrimSpace(r.cfg.GetString("proxy.clearance.user_agent", ""))
	browser := strings.TrimSpace(r.cfg.GetString("proxy.clearance.browser", ""))
	if cookies == "" && userAgent == "" && browser == "" {
		return nil
	}
	if browser == "" {
		browser = browserProfile(userAgent)
	}
	return &ClearanceBundle{
		CFCookies:   cookies,
		UserAgent:   userAgent,
		Browser:     browser,
		RefreshedAt: time.Now().UnixMilli(),
	}
}

func (r *Runtime) getOrBuildFlaresolverrBundle(proxyURL string) (*ClearanceBundle, error) {
	key := affinityKey(proxyURL)
	for {
		r.mu.Lock()
		if bundle, ok := r.bundles[key]; ok && bundle.CFCookies != "" {
			r.mu.Unlock()
			copied := bundle
			return &copied, nil
		}
		if waitCh, ok := r.refreshing[key]; ok {
			r.mu.Unlock()
			<-waitCh
			continue
		}
		waitCh := make(chan struct{})
		r.refreshing[key] = waitCh
		r.mu.Unlock()

		bundle, err := r.fetchFlaresolverrBundle(proxyURL)

		r.mu.Lock()
		if err == nil && bundle != nil {
			r.bundles[key] = *bundle
		}
		waitCh = r.refreshing[key]
		delete(r.refreshing, key)
		r.mu.Unlock()

		close(waitCh)
		if err != nil {
			return nil, err
		}
		return bundle, nil
	}
}

func (r *Runtime) refreshFlaresolverrBundles() {
	items := r.affinityItems()
	type result struct {
		key    string
		bundle *ClearanceBundle
	}
	results := make(chan result, len(items))
	var wg sync.WaitGroup
	for _, proxyURL := range items {
		wg.Add(1)
		go func(proxyURL string) {
			defer wg.Done()
			bundle, err := r.fetchFlaresolverrBundle(proxyURL)
			if err != nil || bundle == nil {
				return
			}
			results <- result{key: affinityKey(proxyURL), bundle: bundle}
		}(proxyURL)
	}
	wg.Wait()
	close(results)

	if len(results) == 0 {
		return
	}

	r.mu.Lock()
	for item := range results {
		r.bundles[item.key] = *item.bundle
	}
	r.mu.Unlock()
}

func (r *Runtime) fetchFlaresolverrBundle(proxyURL string) (*ClearanceBundle, error) {
	fsURL := strings.TrimSpace(r.cfg.GetString("proxy.clearance.flaresolverr_url", ""))
	timeoutSec := r.cfg.GetInt("proxy.clearance.timeout_sec", 60)
	if fsURL == "" {
		return nil, nil
	}
	if timeoutSec <= 0 {
		timeoutSec = 60
	}

	payload := map[string]any{
		"cmd":        "request.get",
		"url":        "https://grok.com",
		"maxTimeout": timeoutSec * 1000,
	}
	if strings.TrimSpace(proxyURL) != "" {
		payload["proxy"] = map[string]any{"url": strings.TrimSpace(proxyURL)}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(fsURL, "/")+"/v1", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: time.Duration(timeoutSec+30) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payloadResp struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		Solution struct {
			UserAgent string `json:"userAgent"`
			Cookies   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		} `json:"solution"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payloadResp); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("flaresolverr returned %d: %s", resp.StatusCode, payloadResp.Message)
	}
	if payloadResp.Status != "ok" {
		if strings.TrimSpace(payloadResp.Message) == "" {
			return nil, fmt.Errorf("flaresolverr returned status=%s", payloadResp.Status)
		}
		return nil, fmt.Errorf("flaresolverr returned status=%s: %s", payloadResp.Status, payloadResp.Message)
	}
	cookies := make([]string, 0, len(payloadResp.Solution.Cookies))
	for _, item := range payloadResp.Solution.Cookies {
		if strings.TrimSpace(item.Name) == "" {
			continue
		}
		cookies = append(cookies, fmt.Sprintf("%s=%s", item.Name, item.Value))
	}
	if len(cookies) == 0 {
		return nil, nil
	}
	ua := strings.TrimSpace(payloadResp.Solution.UserAgent)
	return &ClearanceBundle{
		CFCookies:   strings.Join(cookies, "; "),
		UserAgent:   ua,
		Browser:     browserProfile(ua),
		RefreshedAt: time.Now().UnixMilli(),
	}, nil
}

func (r *Runtime) affinityItems() []string {
	seen := map[string]struct{}{}
	items := make([]string, 0, 8)
	appendUnique := func(values ...string) {
		for _, value := range values {
			trimmed := strings.TrimSpace(value)
			key := affinityKey(trimmed)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, trimmed)
		}
	}

	mode := r.cfg.GetString("proxy.egress.mode", "direct")
	switch mode {
	case "single_proxy":
		appendUnique(
			r.cfg.GetString("proxy.egress.proxy_url", ""),
			r.cfg.GetString("proxy.egress.resource_proxy_url", ""),
		)
	case "proxy_pool":
		appendUnique(r.cfg.GetStringSlice("proxy.egress.proxy_pool")...)
		appendUnique(r.cfg.GetStringSlice("proxy.egress.resource_proxy_pool")...)
	default:
		appendUnique("")
	}
	if len(items) == 0 {
		return []string{""}
	}
	return items
}

func affinityKey(proxyURL string) string {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		return "direct"
	}
	return key
}

func browserProfile(userAgent string) string {
	match := chromeVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(match) == 2 {
		return "chrome" + match[1]
	}
	return "chrome120"
}
