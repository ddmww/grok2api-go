package xai

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/klauspost/compress/zstd"
)

const (
	defaultBaseURL = "https://grok.com"
)

type Client struct {
	cfg             *config.Service
	proxy           *proxy.Runtime
	uploadOnce      sync.Once
	uploadSem       chan struct{}
	listOnce        sync.Once
	listSem         chan struct{}
	deleteOnce      sync.Once
	deleteSem       chan struct{}
	usageMu         sync.Mutex
	usageSession    *RequestSession
	usageSessionKey string
}

type ChatSession struct {
	parent   *Client
	proxyURL string
	mu       sync.Mutex
	client   *http.Client
}

type RequestSession struct {
	parent   *Client
	proxyURL string
	resource bool
	mu       sync.Mutex
	client   *http.Client
}

type UpstreamError struct {
	Status int
	Body   string
}

type readCloserChain struct {
	io.Reader
	closers []io.Closer
}

type closerFunc func() error

func (fn closerFunc) Close() error { return fn() }

func transportError(err error) error {
	if err == nil {
		return nil
	}
	if upstream, ok := err.(*UpstreamError); ok {
		return upstream
	}
	return &UpstreamError{
		Status: http.StatusBadGateway,
		Body:   err.Error(),
	}
}

func (r *readCloserChain) Close() error {
	var firstErr error
	for _, closer := range r.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (e *UpstreamError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("upstream returned %d", e.Status)
	}
	return fmt.Sprintf("upstream returned %d: %s", e.Status, e.Body)
}

func NewClient(cfg *config.Service, proxyRuntime *proxy.Runtime) *Client {
	return &Client{cfg: cfg, proxy: proxyRuntime}
}

func (c *Client) proxyURL(resource bool) string {
	if c == nil || c.proxy == nil {
		return ""
	}
	return strings.TrimSpace(c.proxy.ProxyURL(resource))
}

func (c *Client) newRequestSession(resource bool) (*RequestSession, error) {
	proxyURL := c.proxyURL(resource)
	client, err := c.proxy.NewSessionClientForProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	return &RequestSession{
		parent:   c,
		proxyURL: proxyURL,
		resource: resource,
		client:   client,
	}, nil
}

func (c *Client) NewRequestSession(resource bool) (*RequestSession, error) {
	return c.newRequestSession(resource)
}

func (c *Client) usageSessionAffinityKey() string {
	return "usage:" + c.proxyURL(false)
}

func (c *Client) SharedUsageSession() (*RequestSession, error) {
	desiredKey := c.usageSessionAffinityKey()

	c.usageMu.Lock()
	session := c.usageSession
	if session != nil && c.usageSessionKey == desiredKey {
		c.usageMu.Unlock()
		return session, nil
	}
	c.usageMu.Unlock()

	newSession, err := c.newRequestSession(false)
	if err != nil {
		return nil, err
	}

	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	if c.usageSession != nil && c.usageSessionKey == desiredKey {
		newSession.Close()
		return c.usageSession, nil
	}
	oldSession := c.usageSession
	c.usageSession = newSession
	c.usageSessionKey = desiredKey
	if oldSession != nil {
		oldSession.Close()
	}
	return newSession, nil
}

func (c *Client) CloseSharedUsageSession() {
	c.usageMu.Lock()
	session := c.usageSession
	c.usageSession = nil
	c.usageSessionKey = ""
	c.usageMu.Unlock()
	if session != nil {
		session.Close()
	}
}

func (c *Client) acquireUpload(ctx context.Context) (func(), error) {
	return acquireSemaphore(ctx, c.uploadSemaphore())
}

func (c *Client) acquireList(ctx context.Context) (func(), error) {
	return acquireSemaphore(ctx, c.listSemaphore())
}

func (c *Client) acquireDelete(ctx context.Context) (func(), error) {
	return acquireSemaphore(ctx, c.deleteSemaphore())
}

func (c *Client) uploadSemaphore() chan struct{} {
	c.uploadOnce.Do(func() {
		size := c.cfg.GetInt("batch.asset_upload_concurrency", 8)
		if size <= 0 {
			size = 8
		}
		c.uploadSem = make(chan struct{}, size)
	})
	return c.uploadSem
}

func (c *Client) listSemaphore() chan struct{} {
	c.listOnce.Do(func() {
		size := c.cfg.GetInt("batch.asset_list_concurrency", 8)
		if size <= 0 {
			size = 8
		}
		c.listSem = make(chan struct{}, size)
	})
	return c.listSem
}

func (c *Client) deleteSemaphore() chan struct{} {
	c.deleteOnce.Do(func() {
		size := c.cfg.GetInt("batch.asset_delete_concurrency", 8)
		if size <= 0 {
			size = 8
		}
		c.deleteSem = make(chan struct{}, size)
	})
	return c.deleteSem
}

func acquireSemaphore(ctx context.Context, sem chan struct{}) (func(), error) {
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) withConfigTimeout(ctx context.Context, path string, fallback int) (context.Context, context.CancelFunc) {
	seconds := c.cfg.GetInt(path, fallback)
	if seconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

func (c *Client) baseURL() string {
	if c == nil || c.cfg == nil {
		return defaultBaseURL
	}
	if value := strings.TrimSpace(c.cfg.GetString("proxy.upstream.base_url", "")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return defaultBaseURL
}

func (c *Client) endpoint(path string) string {
	return c.baseURL() + path
}

func (c *Client) buildHeaders(proxyURL, token, contentType, origin, referer string) http.Header {
	bundle, _ := c.proxy.Clearance(proxyURL)
	return buildRequestHeaders(c.cfg, token, contentType, origin, referer, bundle)
}

func (c *Client) do(ctx context.Context, method, urlValue, token string, body []byte, resource bool) (*http.Response, error) {
	client, proxyKey, err := c.proxy.Client(resource)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, urlValue, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers := c.buildHeaders(proxyKey, token, "application/json", "https://grok.com", "https://grok.com/")
	request.Header = headers
	response, err := client.Do(request)
	if err != nil {
		c.proxy.Reset(proxyKey)
		return nil, transportError(err)
	}
	if isResettableStatus(c.cfg, response.StatusCode) {
		c.proxy.Reset(proxyKey)
	}
	if err := decodeResponseBody(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response, nil
}

func decodeResponseBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding == "" {
		return nil
	}

	parts := strings.Split(encoding, ",")
	reader := io.Reader(resp.Body)
	closers := []io.Closer{resp.Body}

	for index := len(parts) - 1; index >= 0; index-- {
		part := strings.ToLower(strings.TrimSpace(parts[index]))
		switch part {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, err := gzip.NewReader(reader)
			if err != nil {
				return err
			}
			reader = gzipReader
			closers = append([]io.Closer{gzipReader}, closers...)
		case "deflate":
			zlibReader, err := zlib.NewReader(reader)
			if err != nil {
				return err
			}
			reader = zlibReader
			closers = append([]io.Closer{zlibReader}, closers...)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			zstdReader, err := zstd.NewReader(reader)
			if err != nil {
				return err
			}
			reader = zstdReader
			closers = append([]io.Closer{closerFunc(func() error {
				zstdReader.Close()
				return nil
			})}, closers...)
		default:
			return fmt.Errorf("unsupported content-encoding: %s", part)
		}
	}

	resp.Body = &readCloserChain{Reader: reader, closers: closers}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

func (c *Client) ChatStream(ctx context.Context, token string, payload map[string]any) (<-chan string, <-chan error) {
	session, err := c.NewChatSession()
	if err != nil {
		out := make(chan string)
		errCh := make(chan error, 1)
		close(out)
		errCh <- err
		close(errCh)
		return out, errCh
	}
	lines, sourceErrCh := session.ChatStream(ctx, token, payload)
	out := make(chan string, 32)
	errCh := make(chan error, 1)
	go func() {
		defer session.Close()
		defer close(out)
		defer close(errCh)
		for line := range lines {
			out <- line
		}
		if err := <-sourceErrCh; err != nil {
			errCh <- err
		}
	}()
	return out, errCh
}

func (c *Client) NewChatSession() (*ChatSession, error) {
	proxyURL := strings.TrimSpace(c.proxy.ProxyURL(false))
	client, err := c.proxy.NewSessionClientForProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	return &ChatSession{
		parent:   c,
		proxyURL: proxyURL,
		client:   client,
	}, nil
}

func (s *ChatSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	client := s.client
	s.client = nil
	s.mu.Unlock()
	closeHTTPClient(client)
}

func (s *ChatSession) reset() error {
	if s == nil || s.parent == nil {
		return nil
	}
	client, err := s.parent.proxy.NewSessionClientForProxyURL(s.proxyURL)
	if err != nil {
		return err
	}
	s.mu.Lock()
	oldClient := s.client
	s.client = client
	s.mu.Unlock()
	closeHTTPClient(oldClient)
	return nil
}

func (s *ChatSession) currentClient() *http.Client {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func closeHTTPClient(client *http.Client) {
	if client == nil {
		return
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func (s *RequestSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	client := s.client
	s.client = nil
	s.mu.Unlock()
	closeHTTPClient(client)
}

func (s *RequestSession) reset() error {
	if s == nil || s.parent == nil {
		return nil
	}
	client, err := s.parent.proxy.NewSessionClientForProxyURL(s.proxyURL)
	if err != nil {
		return err
	}
	s.mu.Lock()
	oldClient := s.client
	s.client = client
	s.mu.Unlock()
	closeHTTPClient(oldClient)
	return nil
}

func (s *RequestSession) currentClient() *http.Client {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *RequestSession) doRequest(ctx context.Context, method, urlValue, token, contentType, origin, referer string, body io.Reader) (*http.Response, error) {
	client := s.currentClient()
	if client == nil {
		return nil, errors.New("request session is closed")
	}
	request, err := http.NewRequestWithContext(ctx, method, urlValue, body)
	if err != nil {
		return nil, err
	}
	request.Header = s.parent.buildHeaders(s.proxyURL, token, contentType, origin, referer)
	response, err := client.Do(request)
	if err != nil {
		_ = s.reset()
		return nil, transportError(err)
	}
	if isResettableStatus(s.parent.cfg, response.StatusCode) {
		if resetErr := s.reset(); resetErr != nil {
			response.Body.Close()
			return nil, resetErr
		}
	}
	if err := decodeResponseBody(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response, nil
}

func (s *RequestSession) postJSON(ctx context.Context, path, token string, payload map[string]any, referer string) (map[string]any, error) {
	data, _ := json.Marshal(payload)
	resp, err := s.doRequest(ctx, http.MethodPost, s.parent.endpoint(path), token, "application/json", "https://grok.com", referer, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *RequestSession) FetchQuotaProbe(ctx context.Context, token string) (account.QuotaWindow, error) {
	body, err := s.postJSON(ctx, "/rest/rate-limits", token, map[string]any{"modelName": "auto"}, "https://grok.com/")
	if err != nil {
		return account.QuotaWindow{}, err
	}
	window, ok := parseQuotaResponse(body)
	if !ok {
		return account.QuotaWindow{}, errors.New("rate limits returned no quota data")
	}
	return window, nil
}

func (s *RequestSession) FetchDetailedQuotas(ctx context.Context, token, pool string, seed map[string]account.QuotaWindow) (map[string]account.QuotaWindow, error) {
	out := map[string]account.QuotaWindow{}
	for key, value := range seed {
		out[key] = value
	}
	type quotaResult struct {
		mode   string
		window account.QuotaWindow
		err    error
	}
	modes := account.SupportedModes(pool)
	results := make(chan quotaResult, len(modes))
	pending := 0
	for _, mode := range modes {
		if _, ok := out[mode]; ok {
			continue
		}
		pending++
		go func(mode string) {
			body, err := s.postJSON(ctx, "/rest/rate-limits", token, map[string]any{"modelName": mode}, "https://grok.com/")
			if err != nil {
				results <- quotaResult{mode: mode, err: err}
				return
			}
			window, ok := parseQuotaResponse(body)
			if !ok {
				results <- quotaResult{mode: mode, err: errors.New("rate limits returned no quota data")}
				return
			}
			results <- quotaResult{mode: mode, window: window}
		}(mode)
	}

	var lastErr error
	for index := 0; index < pending; index++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			if result.err != nil {
				if lastErr == nil {
					lastErr = result.err
				}
				continue
			}
			out[result.mode] = result.window
		}
	}
	if len(out) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("rate limits returned no quota data")
	}
	return out, lastErr
}

func (s *ChatSession) ChatStream(ctx context.Context, token string, payload map[string]any) (<-chan string, <-chan error) {
	out := make(chan string, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		data, _ := json.Marshal(payload)
		client := s.currentClient()
		if client == nil {
			errCh <- errors.New("chat session is closed")
			return
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.parent.endpoint("/rest/app-chat/conversations/new"), bytes.NewReader(data))
		if err != nil {
			errCh <- err
			return
		}
		request.Header = s.parent.buildHeaders(s.proxyURL, token, "application/json", "https://grok.com", "https://grok.com/")
		resp, err := client.Do(request)
		if err != nil {
			_ = s.reset()
			errCh <- transportError(err)
			return
		}
		if isResettableStatus(s.parent.cfg, resp.StatusCode) {
			if resetErr := s.reset(); resetErr != nil {
				resp.Body.Close()
				errCh <- resetErr
				return
			}
		}
		if err := decodeResponseBody(resp); err != nil {
			resp.Body.Close()
			errCh <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			errCh <- &UpstreamError{Status: resp.StatusCode, Body: string(body)}
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
		for scanner.Scan() {
			select {
			case out <- scanner.Text():
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = s.reset()
			errCh <- transportError(err)
		}
	}()
	return out, errCh
}

func (s *ChatSession) postJSON(ctx context.Context, path, token string, payload map[string]any) (map[string]any, error) {
	client := s.currentClient()
	if client == nil {
		return nil, errors.New("chat session is closed")
	}
	data, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.parent.endpoint(path), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header = s.parent.buildHeaders(s.proxyURL, token, "application/json", "https://grok.com", "https://grok.com/")
	resp, err := client.Do(request)
	if err != nil {
		_ = s.reset()
		return nil, transportError(err)
	}
	if isResettableStatus(s.parent.cfg, resp.StatusCode) {
		if resetErr := s.reset(); resetErr != nil {
			resp.Body.Close()
			return nil, resetErr
		}
	}
	if err := decodeResponseBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ChatSession) FetchQuotaProbe(ctx context.Context, token string) (account.QuotaWindow, error) {
	body, err := s.postJSON(ctx, "/rest/rate-limits", token, map[string]any{"modelName": "auto"})
	if err != nil {
		return account.QuotaWindow{}, err
	}
	window, ok := parseQuotaResponse(body)
	if !ok {
		return account.QuotaWindow{}, errors.New("rate limits returned no quota data")
	}
	return window, nil
}

func (s *ChatSession) FetchDetailedQuotas(ctx context.Context, token, pool string, seed map[string]account.QuotaWindow) (map[string]account.QuotaWindow, error) {
	out := map[string]account.QuotaWindow{}
	for key, value := range seed {
		out[key] = value
	}
	type quotaResult struct {
		mode   string
		window account.QuotaWindow
		err    error
	}
	modes := account.SupportedModes(pool)
	results := make(chan quotaResult, len(modes))
	pending := 0
	for _, mode := range modes {
		if _, ok := out[mode]; ok {
			continue
		}
		pending++
		go func(mode string) {
			body, err := s.postJSON(ctx, "/rest/rate-limits", token, map[string]any{"modelName": mode})
			if err != nil {
				results <- quotaResult{mode: mode, err: err}
				return
			}
			window, ok := parseQuotaResponse(body)
			if !ok {
				results <- quotaResult{mode: mode, err: errors.New("rate limits returned no quota data")}
				return
			}
			results <- quotaResult{mode: mode, window: window}
		}(mode)
	}

	var lastErr error
	for index := 0; index < pending; index++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			if result.err != nil {
				if lastErr == nil {
					lastErr = result.err
				}
				continue
			}
			out[result.mode] = result.window
		}
	}
	if len(out) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("rate limits returned no quota data")
	}
	return out, lastErr
}

func (s *RequestSession) SetBirthDate(ctx context.Context, token string) error {
	ctx, cancel := s.parent.withConfigTimeout(ctx, "nsfw.timeout", 60)
	defer cancel()
	payload := map[string]any{"birthDate": time.Now().AddDate(-25, 0, 0).UTC().Format("2006-01-02T15:04:05.000Z")}
	data, _ := json.Marshal(payload)
	resp, err := s.doRequest(ctx, http.MethodPost, s.parent.endpoint("/rest/auth/set-birth-date"), token, "application/json", "https://grok.com", "https://grok.com/", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func (s *RequestSession) SetNSFW(ctx context.Context, token string, enabled bool) error {
	ctx, cancel := s.parent.withConfigTimeout(ctx, "nsfw.timeout", 60)
	defer cancel()
	value := byte(0)
	if enabled {
		value = 1
	}
	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	protobuf := append([]byte{0x0a, 0x02, 0x10, value, 0x12, byte(len(inner))}, inner...)
	frame := append([]byte{0x00, 0x00, 0x00, 0x00, byte(len(protobuf))}, protobuf...)
	resp, err := s.doRequest(ctx, http.MethodPost, s.parent.endpoint("/auth_mgmt.AuthManagement/UpdateUserFeatureControls"), token, "application/grpc-web+proto", "https://grok.com", "https://grok.com/?_s=data", bytes.NewReader(frame))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func (s *RequestSession) ListAssets(ctx context.Context, token string) ([]map[string]any, error) {
	ctx, cancel := s.parent.withConfigTimeout(ctx, "asset.list_timeout", 60)
	defer cancel()
	release, err := s.parent.acquireList(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	resp, err := s.doRequest(ctx, http.MethodGet, s.parent.endpoint("/rest/assets"), token, "application/json", "https://grok.com", "https://grok.com/", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, key := range []string{"assets", "items"} {
		if raw, ok := payload[key].([]any); ok {
			for _, item := range raw {
				if mapped, ok := item.(map[string]any); ok {
					items = append(items, mapped)
				}
			}
		}
	}
	return items, nil
}

func (s *RequestSession) DeleteAsset(ctx context.Context, token, assetID string) error {
	ctx, cancel := s.parent.withConfigTimeout(ctx, "asset.delete_timeout", 60)
	defer cancel()
	release, err := s.parent.acquireDelete(ctx)
	if err != nil {
		return err
	}
	defer release()

	resp, err := s.doRequest(ctx, http.MethodDelete, s.parent.endpoint("/rest/assets-metadata")+"/"+assetID, token, "application/json", "https://grok.com", "https://grok.com/", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func modePayload(mode string) []byte {
	data, _ := json.Marshal(map[string]any{"modelName": mode})
	return data
}

func parseQuotaResponse(body map[string]any) (account.QuotaWindow, bool) {
	remaining, ok := body["remainingQueries"]
	if !ok {
		return account.QuotaWindow{}, false
	}
	total, _ := body["totalQueries"]
	window, _ := body["windowSizeSeconds"]
	return account.QuotaWindow{
		Remaining:     int(asFloat(remaining)),
		Total:         int(asFloat(total)),
		WindowSeconds: int(asFloat(window)),
		ResetAt:       time.Now().Add(time.Duration(int(asFloat(window))) * time.Second).UnixMilli(),
		SyncedAt:      time.Now().UnixMilli(),
		Source:        account.QuotaSourceReal,
	}, true
}

func asFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func (c *Client) FetchAllQuotas(ctx context.Context, token, pool string) (map[string]account.QuotaWindow, error) {
	out := map[string]account.QuotaWindow{}
	for _, mode := range account.SupportedModes(pool) {
		resp, err := c.do(ctx, http.MethodPost, c.endpoint("/rest/rate-limits"), token, modePayload(mode), false)
		if err != nil {
			return nil, err
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, &UpstreamError{Status: resp.StatusCode, Body: string(bodyBytes)}
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			return nil, err
		}
		if window, ok := parseQuotaResponse(body); ok {
			out[mode] = window
		}
	}
	if len(out) == 0 {
		return nil, errors.New("rate limits returned no quota data")
	}
	return out, nil
}

func (c *Client) SetBirthDate(ctx context.Context, token string) error {
	session, err := c.newRequestSession(false)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.SetBirthDate(ctx, token)
}

func (c *Client) SetNSFW(ctx context.Context, token string, enabled bool) error {
	session, err := c.newRequestSession(false)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.SetNSFW(ctx, token, enabled)
}

func (c *Client) ListAssets(ctx context.Context, token string) ([]map[string]any, error) {
	session, err := c.newRequestSession(true)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	return session.ListAssets(ctx, token)
}

func (c *Client) DeleteAsset(ctx context.Context, token, assetID string) error {
	session, err := c.newRequestSession(true)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.DeleteAsset(ctx, token, assetID)
}
