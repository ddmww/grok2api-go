package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/config"
	xproxy "golang.org/x/net/proxy"
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

	insecure := r.cfg.GetBool("proxy.egress.skip_ssl_verify", false)
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, "", err
		}
		if strings.HasPrefix(parsed.Scheme, "socks5") {
			dialer, err := xproxy.FromURL(parsed, xproxy.Direct)
			if err != nil {
				return nil, "", err
			}
			if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
				transport.DialContext = contextDialer.DialContext
			} else {
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				}
			}
		} else {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	client := &http.Client{Transport: transport}
	r.mu.Lock()
	r.cache[key] = client
	r.mu.Unlock()
	return client, proxyURL, nil
}
