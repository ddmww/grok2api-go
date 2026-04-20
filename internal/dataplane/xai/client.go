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
	"time"

	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const (
	defaultBaseURL = "https://grok.com"
)

type Client struct {
	cfg   *config.Service
	proxy *proxy.Runtime
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

func (c *Client) buildHeaders(token, contentType, origin, referer string) http.Header {
	return buildRequestHeaders(c.cfg, token, contentType, origin, referer)
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
	headers := c.buildHeaders(token, "application/json", "https://grok.com", "https://grok.com/")
	request.Header = headers
	response, err := client.Do(request)
	if err != nil {
		return nil, err
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
	out := make(chan string, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		data, _ := json.Marshal(payload)
		resp, err := c.do(ctx, http.MethodPost, c.endpoint("/rest/app-chat/conversations/new"), token, data, false)
		if err != nil {
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
			errCh <- err
		}
	}()
	return out, errCh
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
	payload := map[string]any{"birthDate": time.Now().AddDate(-25, 0, 0).UTC().Format("2006-01-02T15:04:05.000Z")}
	data, _ := json.Marshal(payload)
	resp, err := c.do(ctx, http.MethodPost, c.endpoint("/rest/auth/set-birth-date"), token, data, false)
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

func (c *Client) SetNSFW(ctx context.Context, token string, enabled bool) error {
	value := byte(0)
	if enabled {
		value = 1
	}
	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	protobuf := append([]byte{0x0a, 0x02, 0x10, value, 0x12, byte(len(inner))}, inner...)
	frame := append([]byte{0x00, 0x00, 0x00, 0x00, byte(len(protobuf))}, protobuf...)
	client, proxyKey, err := c.proxy.Client(false)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/auth_mgmt.AuthManagement/UpdateUserFeatureControls"), bytes.NewReader(frame))
	if err != nil {
		return err
	}
	request.Header = c.buildHeaders(token, "application/grpc-web+proto", "https://grok.com", "https://grok.com/?_s=data")
	request.Header.Set("x-grpc-web", "1")
	request.Header.Set("x-user-agent", "grpc-web-javascript/0.1")
	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	if isResettableStatus(c.cfg, resp.StatusCode) {
		c.proxy.Reset(proxyKey)
	}
	if err := decodeResponseBody(resp); err != nil {
		resp.Body.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func (c *Client) ListAssets(ctx context.Context, token string) ([]map[string]any, error) {
	client, proxyKey, err := c.proxy.Client(true)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/rest/assets"), nil)
	if err != nil {
		return nil, err
	}
	request.Header = c.buildHeaders(token, "application/json", "https://grok.com", "https://grok.com/")
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if isResettableStatus(c.cfg, resp.StatusCode) {
		c.proxy.Reset(proxyKey)
	}
	if err := decodeResponseBody(resp); err != nil {
		resp.Body.Close()
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

func (c *Client) DeleteAsset(ctx context.Context, token, assetID string) error {
	client, proxyKey, err := c.proxy.Client(true)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint("/rest/assets-metadata")+"/"+assetID, nil)
	if err != nil {
		return err
	}
	request.Header = c.buildHeaders(token, "application/json", "https://grok.com", "https://grok.com/")
	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	if isResettableStatus(c.cfg, resp.StatusCode) {
		c.proxy.Reset(proxyKey)
	}
	if err := decodeResponseBody(resp); err != nil {
		resp.Body.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}
