package xai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/platform/config"
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

func sanitizeToken(token string) string {
	if strings.HasPrefix(token, "sso=") {
		return strings.TrimPrefix(token, "sso=")
	}
	return token
}

func (c *Client) buildCookie(token string) string {
	cookie := fmt.Sprintf("sso=%s; sso-rw=%s", sanitizeToken(token), sanitizeToken(token))
	if extra := strings.TrimSpace(c.cfg.GetString("proxy.clearance.cf_cookies", "")); extra != "" {
		cookie += "; " + extra
	}
	return cookie
}

func (c *Client) buildHeaders(token, contentType, origin, referer string) http.Header {
	if contentType == "" {
		contentType = "application/json"
	}
	if origin == "" {
		origin = "https://grok.com"
	}
	if referer == "" {
		referer = "https://grok.com/"
	}
	userAgent := c.cfg.GetString("proxy.clearance.user_agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
	statsig := "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGlsZE5vZGVzJyk="
	if c.cfg.GetBool("features.dynamic_statsig", false) {
		statsig = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("e:TypeError:%d", rand.Intn(999999))))
	}
	headers := http.Header{}
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Content-Type", contentType)
	headers.Set("Origin", origin)
	headers.Set("Referer", referer)
	headers.Set("Priority", "u=1, i")
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("Sec-Fetch-Site", "same-site")
	headers.Set("User-Agent", userAgent)
	headers.Set("Cookie", c.buildCookie(token))
	headers.Set("x-statsig-id", statsig)
	headers.Set("x-xai-request-id", fmt.Sprintf("%d", time.Now().UnixNano()))
	return headers
}

func (c *Client) do(ctx context.Context, method, urlValue, token string, body []byte, resource bool) (*http.Response, error) {
	client, _, err := c.proxy.Client(resource)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, urlValue, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers := c.buildHeaders(token, "application/json", "https://grok.com", "https://grok.com/")
	request.Header = headers
	return client.Do(request)
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
	client, _, err := c.proxy.Client(false)
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func (c *Client) ListAssets(ctx context.Context, token string) ([]map[string]any, error) {
	client, _, err := c.proxy.Client(true)
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
	client, _, err := c.proxy.Client(true)
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}
