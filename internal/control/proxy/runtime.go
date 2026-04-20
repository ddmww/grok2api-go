package proxy

import (
	"net/http"
	"strings"
	"sync"

	"github.com/ddmww/grok2api-go/internal/platform/config"
)

type Runtime struct {
	cfg   *config.Service
	mu    sync.Mutex
	index int
	cache map[string]*http.Client
}

func NewRuntime(cfg *config.Service) *Runtime {
	return &Runtime{cfg: cfg, cache: map[string]*http.Client{}}
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
	key := proxyURL
	if key == "" {
		key = "direct"
	}
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

func (r *Runtime) Reset(proxyURL string) {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		key = "direct"
	}

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
