package xai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
)

var (
	imageURLPattern = regexp.MustCompile(`https?://[^\s<>"']+\.(?:png|jpg|jpeg|webp)(?:\?[^\s<>"']*)?`)
	videoURLPattern = regexp.MustCompile(`https?://[^\s<>"']+\.(?:mp4|mov|webm)(?:\?[^\s<>"']*)?`)
	dataURIPattern  = regexp.MustCompile(`^data:([^;,]+);base64,(.+)$`)
	xUserIDPattern  = regexp.MustCompile(`(?:^|;\s*)x-userid=([^;]+)`)
	imagePathIDRe   = regexp.MustCompile(`/images/([a-f0-9-]+)\.(png|jpg|jpeg)`)
)

type UploadedAsset struct {
	FileID  string
	FileURI string
	URL     string
}

type GeneratedImage struct {
	URL      string
	BlobB64  string
	Progress int
	ImageID  string
	Stage    string
	IsFinal  bool
}

type ImageGenerationMeta struct {
	Backend       string
	UsedFallback  bool
	SawRateLimit  bool
	SelectedStage string
}

type ImageRequest struct {
	Model    string
	Mode     string
	Prompt   string
	N        int
	Size     string
	Messages []map[string]any
}

type ImageEditRequest struct {
	Prompt string
	Inputs []string
	Size   string
}

type VideoRequest struct {
	Model           string
	Prompt          string
	Seconds         int
	Size            string
	ResolutionName  string
	Preset          string
	InputReferences []string
}

type VideoResult struct {
	URL       string
	PostID    string
	AssetID   string
	Progress  int
	Thumbnail string
}

func uniqueURLs(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func sanitizeUploadName(name, fallback string) string {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "." || name == "/" || name == `\` || name == "" {
		return fallback
	}
	return name
}

func inferMimeFromName(name, fallback string) string {
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); value != "" {
		return value
	}
	return fallback
}

func parseDataURI(input string) (filename, mimeType, b64 string, err error) {
	match := dataURIPattern.FindStringSubmatch(strings.TrimSpace(input))
	if len(match) != 3 {
		return "", "", "", fmt.Errorf("file input must be a URL or base64 data URI")
	}
	mimeType = strings.TrimSpace(match[1])
	b64 = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		default:
			return r
		}
	}, match[2])
	if mimeType == "" || b64 == "" {
		return "", "", "", fmt.Errorf("invalid data URI payload")
	}
	exts, _ := mime.ExtensionsByType(mimeType)
	ext := ".bin"
	if len(exts) > 0 {
		ext = exts[0]
	}
	return "file" + ext, mimeType, b64, nil
}

func isRemoteURL(input string) bool {
	parsed, err := url.Parse(strings.TrimSpace(input))
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func resolveAssetReference(token, fileID, fileURI string) string {
	if strings.TrimSpace(fileURI) != "" {
		if strings.HasPrefix(fileURI, "http://") || strings.HasPrefix(fileURI, "https://") {
			return fileURI
		}
		return "https://assets.grok.com/" + strings.TrimLeft(fileURI, "/")
	}
	cookie := buildSSOCookie(nil, token, nil)
	if match := xUserIDPattern.FindStringSubmatch(cookie); len(match) == 2 && fileID != "" {
		return fmt.Sprintf("https://assets.grok.com/users/%s/%s/content", match[1], fileID)
	}
	return ""
}

func resolveAssetReferenceWithConfig(cfg *config.Service, token, fileID, fileURI string) string {
	if strings.TrimSpace(fileURI) != "" {
		if strings.HasPrefix(fileURI, "http://") || strings.HasPrefix(fileURI, "https://") {
			return fileURI
		}
		return "https://assets.grok.com/" + strings.TrimLeft(fileURI, "/")
	}
	cookie := buildSSOCookie(cfg, token, nil)
	if match := xUserIDPattern.FindStringSubmatch(cookie); len(match) == 2 && fileID != "" {
		return fmt.Sprintf("https://assets.grok.com/users/%s/%s/content", match[1], fileID)
	}
	return ""
}

func (c *Client) doRequest(ctx context.Context, method, urlValue, token, contentType, origin, referer string, body io.Reader, resource bool) (*http.Response, error) {
	client, proxyKey, err := c.proxy.Client(resource)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, urlValue, body)
	if err != nil {
		return nil, err
	}
	request.Header = c.buildHeaders(proxyKey, token, contentType, origin, referer)
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

func (c *Client) postJSON(ctx context.Context, endpoint, token string, payload map[string]any, referer string) (map[string]any, error) {
	data, _ := json.Marshal(payload)
	resp, err := c.doRequest(ctx, http.MethodPost, c.endpoint(endpoint), token, "application/json", "https://grok.com", referer, bytes.NewReader(data), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	var out map[string]any
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) UploadFromInput(ctx context.Context, token, input string) (*UploadedAsset, error) {
	ctx, cancel := c.withConfigTimeout(ctx, "asset.upload_timeout", 60)
	defer cancel()
	release, err := c.acquireUpload(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	input = strings.TrimSpace(input)
	switch {
	case input == "":
		return nil, fmt.Errorf("empty file input")
	case isRemoteURL(input):
		data, contentType, err := c.DownloadContent(ctx, token, input)
		if err != nil {
			return nil, err
		}
		filename := sanitizeUploadName(filepath.Base(strings.Split(input, "?")[0]), "download.bin")
		b64 := base64.StdEncoding.EncodeToString(data)
		return c.uploadBase64(ctx, token, filename, contentType, b64)
	default:
		filename, mimeType, b64, err := parseDataURI(input)
		if err != nil {
			return nil, err
		}
		return c.uploadBase64(ctx, token, filename, mimeType, b64)
	}
}

func (c *Client) uploadBase64(ctx context.Context, token, filename, mimeType, b64 string) (*UploadedAsset, error) {
	payload := map[string]any{
		"fileName":     sanitizeUploadName(filename, "upload.bin"),
		"fileMimeType": mimeType,
		"content":      b64,
	}
	result, err := c.postJSON(ctx, "/rest/app-chat/upload-file", token, payload, "https://grok.com/")
	if err != nil {
		return nil, err
	}
	fileID, _ := result["fileMetadataId"].(string)
	if fileID == "" {
		fileID, _ = result["fileId"].(string)
	}
	fileURI, _ := result["fileUri"].(string)
	return &UploadedAsset{
		FileID:  fileID,
		FileURI: fileURI,
		URL:     resolveAssetReferenceWithConfig(c.cfg, token, fileID, fileURI),
	}, nil
}

func (c *Client) CreateMediaPost(ctx context.Context, token, mediaType, mediaURL, prompt, referer string) (map[string]any, error) {
	ctx, cancel := c.withConfigTimeout(ctx, "video.timeout", 60)
	defer cancel()
	payload := map[string]any{"mediaType": mediaType}
	if mediaURL != "" {
		payload["mediaUrl"] = mediaURL
	}
	if prompt != "" {
		payload["prompt"] = prompt
	}
	if referer == "" {
		referer = "https://grok.com/imagine"
	}
	return c.postJSON(ctx, "/rest/media/post/create", token, payload, referer)
}

func (c *Client) CreateMediaLink(ctx context.Context, token, postID string) (map[string]any, error) {
	ctx, cancel := c.withConfigTimeout(ctx, "video.timeout", 60)
	defer cancel()
	return c.postJSON(ctx, "/rest/media/post/create-link", token, map[string]any{
		"postId":   postID,
		"source":   "post-page",
		"platform": "web",
	}, "https://grok.com/")
}

func (c *Client) DownloadContent(ctx context.Context, token, rawURL string) ([]byte, string, error) {
	ctx, cancel := c.withConfigTimeout(ctx, "asset.download_timeout", 60)
	defer cancel()
	target := strings.TrimSpace(rawURL)
	if target == "" {
		return nil, "", fmt.Errorf("empty download URL")
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://assets.grok.com/" + strings.TrimLeft(target, "/")
	}
	resp, err := c.doRequest(ctx, http.MethodGet, target, token, "application/json", "https://grok.com", "https://grok.com/", nil, true)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
	if readErr != nil {
		return nil, "", readErr
	}
	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if contentType == "" {
		contentType = inferContentType(target)
	}
	return body, contentType, nil
}

func inferContentType(rawURL string) string {
	switch strings.ToLower(filepath.Ext(strings.Split(rawURL, "?")[0])) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func buildImagePayload(modelName, prompt, aspectRatio string, count int, temporary, memory bool) map[string]any {
	return map[string]any{
		"temporary":                 temporary,
		"modelName":                 modelName,
		"message":                   prompt,
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"returnRawGrokInXaiRequest": false,
		"enableImageStreaming":      true,
		"imageGenerationCount":      count,
		"forceConcise":              false,
		"toolOverrides":             map[string]any{"imageGen": true},
		"enableSideBySide":          true,
		"sendFinalMetadata":         true,
		"isReasoning":               false,
		"disableTextFollowUps":      true,
		"disableMemory":             !memory,
		"forceSideBySide":           false,
		"responseMetadata": map[string]any{
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{
					"imageGenModelConfig": map[string]any{
						"aspectRatio": aspectRatio,
					},
				},
			},
		},
	}
}

func buildChatLikeImagePayload(customInstruction, mode, prompt string, count int, temporary, memory bool) map[string]any {
	if count <= 0 {
		count = 1
	}
	payload := map[string]any{
		"collectionIds":               []string{},
		"connectors":                  []string{},
		"deviceEnvInfo":               map[string]any{"darkModeEnabled": false, "devicePixelRatio": 2, "screenHeight": 1329, "screenWidth": 2056, "viewportHeight": 1083, "viewportWidth": 2056},
		"disableMemory":               !memory,
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             []string{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []string{},
		"imageGenerationCount":        count,
		"isAsyncChat":                 false,
		"message":                     "Drawing: " + prompt,
		"modeId":                      mode,
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"searchAllConnectors":         false,
		"sendFinalMetadata":           true,
		"temporary":                   temporary,
		"toolOverrides": map[string]any{
			"gmailSearch":           false,
			"googleCalendarSearch":  false,
			"outlookSearch":         false,
			"outlookCalendarSearch": false,
			"googleDriveSearch":     false,
		},
	}
	if custom := strings.TrimSpace(customInstruction); custom != "" {
		payload["customPersonality"] = custom
	}
	return payload
}

func buildImageEditPayload(prompt string, refs []string, parentPostID string, temporary, memory bool) map[string]any {
	return map[string]any{
		"temporary":                 temporary,
		"modelName":                 "imagine-image-edit",
		"message":                   prompt,
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"returnRawGrokInXaiRequest": false,
		"enableImageStreaming":      true,
		"imageGenerationCount":      2,
		"forceConcise":              false,
		"toolOverrides":             map[string]any{"imageGen": true},
		"enableSideBySide":          true,
		"sendFinalMetadata":         true,
		"isReasoning":               false,
		"disableTextFollowUps":      true,
		"disableMemory":             !memory,
		"forceSideBySide":           false,
		"responseMetadata": map[string]any{
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{
					"imageEditModel": "imagine",
					"imageEditModelConfig": map[string]any{
						"imageReferences": refs,
						"parentPostId":    parentPostID,
					},
				},
			},
		},
	}
}

func buildVideoPayload(prompt, parentPostID, aspectRatio, resolutionName, preset string, seconds int, refs []string) map[string]any {
	config := map[string]any{
		"parentPostId":   parentPostID,
		"aspectRatio":    aspectRatio,
		"videoLength":    seconds,
		"resolutionName": resolutionName,
	}
	if len(refs) > 0 {
		config["isVideoEdit"] = false
		config["isReferenceToVideo"] = true
		config["imageReferences"] = refs
	}
	return map[string]any{
		"temporary":        true,
		"modelName":        "grok-3",
		"message":          strings.TrimSpace(prompt + " --mode=" + preset),
		"toolOverrides":    map[string]any{"videoGen": true},
		"enableSideBySide": true,
		"responseMetadata": map[string]any{
			"experiments": []string{},
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{
					"videoGenModelConfig": config,
				},
			},
		},
	}
}

func extractGeneratedImageURLs(payload map[string]any) []string {
	result, _ := payload["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	modelResponse, _ := response["modelResponse"].(map[string]any)
	if modelResponse != nil {
		if items, ok := modelResponse["generatedImageUrls"].([]any); ok {
			out := make([]string, 0, len(items))
			for _, item := range items {
				if value, ok := item.(string); ok && value != "" {
					out = append(out, value)
				}
			}
			if len(out) > 0 {
				return uniqueURLs(out)
			}
		}
	}
	if response != nil {
		if streamResp, ok := response["streamingImageGenerationResponse"].(map[string]any); ok {
			if raw, _ := streamResp["url"].(string); raw != "" {
				return []string{raw}
			}
		}
		if token, _ := response["token"].(string); token != "" {
			return uniqueURLs(imageURLPattern.FindAllString(token, -1))
		}
	}
	return nil
}

func extractModelResponseFileAttachments(payload map[string]any) []string {
	result, _ := payload["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	modelResponse, _ := response["modelResponse"].(map[string]any)
	if modelResponse == nil {
		return nil
	}
	items, ok := modelResponse["fileAttachments"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if value, ok := item.(string); ok && value != "" {
			out = append(out, value)
		}
	}
	return uniqueURLs(out)
}

func extractStreamingEditResponse(payload map[string]any) map[string]any {
	result, _ := payload["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	stream, _ := response["streamingImageGenerationResponse"].(map[string]any)
	return stream
}

func absolutizeAssetURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	return "https://assets.grok.com/" + strings.TrimLeft(raw, "/")
}

func resolveEditFinalURL(cfg *config.Service, token, rawURL, assetID string) string {
	if assetID != "" {
		if resolved := resolveAssetReferenceWithConfig(cfg, token, assetID, ""); resolved != "" {
			return resolved
		}
	}
	if rawURL != "" {
		return absolutizeAssetURL(rawURL)
	}
	return ""
}

func parseImageIndex(value any) (int, bool) {
	index, ok := asInt(value)
	if !ok || index < 0 {
		return 0, false
	}
	return index, true
}

func extractGeneratedVideo(payload map[string]any) VideoResult {
	var result VideoResult
	root, _ := payload["result"].(map[string]any)
	response, _ := root["response"].(map[string]any)
	if response != nil {
		if streamResp, ok := response["streamingVideoGenerationResponse"].(map[string]any); ok {
			if value, _ := streamResp["progress"].(float64); value > 0 {
				result.Progress = int(value)
			}
			if raw, _ := streamResp["url"].(string); raw != "" {
				result.URL = raw
			}
			if raw, _ := streamResp["videoUrl"].(string); raw != "" {
				result.URL = raw
			}
			if raw, _ := streamResp["thumbnailUrl"].(string); raw != "" {
				result.Thumbnail = raw
			}
		}
		if modelResponse, ok := response["modelResponse"].(map[string]any); ok {
			if attachments, ok := modelResponse["fileAttachments"].([]any); ok && len(attachments) > 0 {
				if attachment, ok := attachments[0].(string); ok && attachment != "" {
					result.AssetID = attachment
				}
			}
			if generated, ok := modelResponse["generatedVideoUrls"].([]any); ok && len(generated) > 0 {
				if raw, ok := generated[0].(string); ok && raw != "" {
					result.URL = raw
				}
			}
		}
		if token, _ := response["token"].(string); token != "" && result.URL == "" {
			items := uniqueURLs(videoURLPattern.FindAllString(token, -1))
			if len(items) > 0 {
				result.URL = items[0]
			}
		}
	}
	return result
}

func (c *Client) streamJSON(ctx context.Context, token string, payload map[string]any) ([]map[string]any, error) {
	lines, errCh := c.ChatStream(ctx, token, payload)
	frames := make([]map[string]any, 0, 8)
	for line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || strings.HasPrefix(line, "event:") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err == nil {
			frames = append(frames, frame)
		}
	}
	if err := <-errCh; err != nil {
		return nil, err
	}
	return frames, nil
}

func (c *Client) GenerateImages(ctx context.Context, token string, req ImageRequest) ([]GeneratedImage, string, *ImageGenerationMeta, error) {
	count := req.N
	if count <= 0 {
		count = 1
	}
	backend := c.imageBackend()
	meta := &ImageGenerationMeta{Backend: backend}
	reasoningParts := []string{}
	combined := make([]GeneratedImage, 0, count*2)
	lastErr := error(nil)

	if backend == "auto" || backend == "app_chat" {
		appImages, reasoning, appMeta, err := c.generateImagesViaAppChat(ctx, token, req, count)
		if reasoning != "" {
			reasoningParts = append(reasoningParts, reasoning)
		}
		if appMeta != nil && appMeta.SawRateLimit {
			meta.SawRateLimit = true
		}
		if err == nil {
			combined = append(combined, appImages...)
			if selected, stage := selectFinalOrPartialImages(combined, count); backend == "app_chat" || hasEnoughFinals(combined, count) {
				meta.SelectedStage = stage
				return selected, strings.Join(reasoningParts, "\n"), meta, nil
			}
		} else {
			lastErr = err
			if backend == "app_chat" {
				return nil, strings.Join(reasoningParts, "\n"), meta, err
			}
		}
		meta.UsedFallback = true
		meta.Backend = "websocket"
	}

	if backend == "auto" || backend == "websocket" {
		wsImages, reasoning, wsMeta, err := c.generateImagesViaWebsocket(ctx, token, req, count)
		if reasoning != "" {
			reasoningParts = append(reasoningParts, reasoning)
		}
		if wsMeta != nil && wsMeta.SawRateLimit {
			meta.SawRateLimit = true
		}
		if err == nil {
			combined = append(combined, wsImages...)
			if selected, stage := selectFinalOrPartialImages(combined, count); len(selected) > 0 {
				meta.SelectedStage = stage
				return selected, strings.Join(reasoningParts, "\n"), meta, nil
			}
		} else if lastErr == nil {
			lastErr = err
		}
	}

	if selected, stage := selectFinalOrPartialImages(combined, count); len(selected) > 0 {
		meta.SelectedStage = stage
		return selected, strings.Join(reasoningParts, "\n"), meta, nil
	}
	if lastErr != nil {
		return nil, strings.Join(reasoningParts, "\n"), meta, lastErr
	}
	return nil, strings.Join(reasoningParts, "\n"), meta, &UpstreamError{Status: 502, Body: "Image generation returned no images"}
}

func (c *Client) imageBackend() string {
	switch strings.ToLower(strings.TrimSpace(c.cfg.GetString("image.backend", "auto"))) {
	case "app_chat", "websocket":
		return strings.ToLower(strings.TrimSpace(c.cfg.GetString("image.backend", "auto")))
	default:
		return "auto"
	}
}

func (c *Client) generateImagesViaAppChat(ctx context.Context, token string, req ImageRequest, count int) ([]GeneratedImage, string, *ImageGenerationMeta, error) {
	payload := buildChatLikeImagePayload(
		c.cfg.GetString("features.custom_instruction", ""),
		defaultIfEmpty(strings.TrimSpace(req.Mode), "fast"),
		req.Prompt,
		count,
		c.cfg.GetBool("features.temporary", true),
		c.cfg.GetBool("features.memory", false),
	)
	frames, err := c.streamJSON(ctx, token, payload)
	if err != nil {
		return nil, "", &ImageGenerationMeta{Backend: "app_chat", SawRateLimit: isRateLimitedError(err)}, err
	}
	items, reasoning := collectImageCandidatesFromFrames(frames)
	return items, reasoning, &ImageGenerationMeta{Backend: "app_chat"}, nil
}

func (c *Client) generateImagesViaWebsocket(ctx context.Context, token string, req ImageRequest, count int) ([]GeneratedImage, string, *ImageGenerationMeta, error) {
	if c.baseURL() != defaultBaseURL {
		return c.generateImagesViaWebsocketCompat(ctx, token, req, count)
	}
	return c.generateImagesViaWebsocketLive(ctx, token, req, count)
}

func (c *Client) generateImagesViaWebsocketCompat(ctx context.Context, token string, req ImageRequest, count int) ([]GeneratedImage, string, *ImageGenerationMeta, error) {
	attempts := 1
	attempts += 5
	all := make([]GeneratedImage, 0, count*attempts)
	reasoningParts := []string{}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		frames, err := c.streamJSON(ctx, token, buildImagePayload(
			req.Model,
			req.Prompt,
			resolveImageAspectRatio(req.Size),
			count,
			c.cfg.GetBool("features.temporary", true),
			c.cfg.GetBool("features.memory", false),
		))
		if err != nil {
			lastErr = err
			if isRateLimitedError(err) {
				return nil, strings.Join(reasoningParts, "\n"), &ImageGenerationMeta{Backend: "websocket", SawRateLimit: true}, err
			}
			continue
		}
		items, reasoning := collectImageCandidatesFromFrames(frames)
		if reasoning != "" {
			reasoningParts = append(reasoningParts, reasoning)
		}
		all = append(all, items...)
		if selected, _ := selectFinalOrPartialImages(all, count); len(selected) >= count && hasEnoughFinals(all, count) {
			return selected, strings.Join(reasoningParts, "\n"), &ImageGenerationMeta{Backend: "websocket"}, nil
		}
	}
	if selected, _ := selectFinalOrPartialImages(all, count); len(selected) > 0 {
		return selected, strings.Join(reasoningParts, "\n"), &ImageGenerationMeta{Backend: "websocket"}, nil
	}
	if lastErr != nil {
		return nil, strings.Join(reasoningParts, "\n"), &ImageGenerationMeta{Backend: "websocket"}, lastErr
	}
	return nil, strings.Join(reasoningParts, "\n"), &ImageGenerationMeta{Backend: "websocket"}, fmt.Errorf("image generation returned no images")
}

func (c *Client) generateImagesViaWebsocketLive(ctx context.Context, token string, req ImageRequest, count int) ([]GeneratedImage, string, *ImageGenerationMeta, error) {
	attempts := 1 + 5
	all := make([]GeneratedImage, 0, count*attempts)
	lastErr := error(nil)
	reasoning := ""

	for attempt := 0; attempt < attempts; attempt++ {
		items, err := c.streamImagineWebsocketOnce(ctx, token, req.Prompt, resolveImageAspectRatio(req.Size), count)
		if err != nil {
			lastErr = err
			if isRateLimitedError(err) {
				return nil, reasoning, &ImageGenerationMeta{Backend: "websocket", SawRateLimit: true}, err
			}
			continue
		}
		all = append(all, items...)
		if selected, _ := selectFinalOrPartialImages(all, count); len(selected) >= count && hasEnoughFinals(all, count) {
			return selected, reasoning, &ImageGenerationMeta{Backend: "websocket"}, nil
		}
	}

	if selected, _ := selectFinalOrPartialImages(all, count); len(selected) > 0 {
		return selected, reasoning, &ImageGenerationMeta{Backend: "websocket"}, nil
	}
	if lastErr != nil {
		return nil, reasoning, &ImageGenerationMeta{Backend: "websocket"}, lastErr
	}
	return nil, reasoning, &ImageGenerationMeta{Backend: "websocket"}, fmt.Errorf("image generation returned no images")
}

func (c *Client) streamImagineWebsocketOnce(ctx context.Context, token, prompt, aspectRatio string, count int) ([]GeneratedImage, error) {
	proxyURL := c.proxy.ProxyURL(false)
	bundle, _ := c.proxy.Clearance(proxyURL)
	headers := buildWSHeaders(c.cfg, token, "https://grok.com", bundle)
	wsURL := "wss://grok.com/ws/imagine/listen"

	timeout := time.Duration(c.cfg.GetFloat("image.timeout", 60) * float64(time.Second))
	conn, err := dialImagineWebsocket(ctx, wsURL, headers, timeout, c.cfg, proxyURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	requestID := uuid.NewString()
	payload := map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type": "message",
			"content": []map[string]any{
				{
					"requestId": requestID,
					"text":      prompt,
					"type":      "input_text",
					"properties": map[string]any{
						"section_count":  0,
						"is_kids_mode":   false,
						"enable_nsfw":    c.cfg.GetBool("features.enable_nsfw", true),
						"skip_upsampler": false,
						"is_initial":     false,
						"aspect_ratio":   aspectRatio,
					},
				},
			},
		},
	}
	if err := conn.WriteJSON(payload); err != nil {
		return nil, err
	}

	streamTimeout := time.Duration(c.cfg.GetFloat("image.stream_timeout", 60) * float64(time.Second))
	finalTimeout := time.Duration(c.cfg.GetFloat("image.final_timeout", 15) * float64(time.Second))
	blockedGrace := time.Duration(c.cfg.GetFloat("image.blocked_grace_seconds", 10) * float64(time.Second))
	if blockedGrace < time.Second {
		blockedGrace = time.Second
	}
	if blockedGrace > finalTimeout {
		blockedGrace = finalTimeout
	}
	mediumMinBytes := c.cfg.GetInt("image.medium_min_bytes", 30000)
	finalMinBytes := c.cfg.GetInt("image.final_min_bytes", 100000)

	start := time.Now()
	lastActivity := start
	var mediumReceivedAt time.Time
	finalIDs := map[string]struct{}{}
	items := make([]GeneratedImage, 0, count*2)
	readWindow := 5 * time.Second
	if streamTimeout > 0 && streamTimeout < readWindow {
		readWindow = streamTimeout
	}

	for time.Since(start) < timeout {
		if err := conn.SetReadDeadline(time.Now().Add(readWindow)); err != nil {
			return dedupeGeneratedImages(items), err
		}
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
				now := time.Now()
				if !mediumReceivedAt.IsZero() && len(finalIDs) == 0 && now.Sub(mediumReceivedAt) > blockedGrace {
					break
				}
				if len(finalIDs) > 0 && now.Sub(lastActivity) > 10*time.Second {
					break
				}
				continue
			}
			return dedupeGeneratedImages(items), err
		}

		lastActivity = time.Now()
		switch strings.TrimSpace(asString(msg["type"])) {
		case "image":
			item := classifyWSImage(msg, finalMinBytes, mediumMinBytes)
			if item == nil {
				continue
			}
			if item.Stage == "medium" && mediumReceivedAt.IsZero() {
				mediumReceivedAt = time.Now()
			}
			if item.IsFinal {
				finalIDs[item.ImageID] = struct{}{}
			}
			items = append(items, *item)
			if len(finalIDs) >= count {
				return dedupeGeneratedImages(items), nil
			}
			if !mediumReceivedAt.IsZero() && len(finalIDs) == 0 && time.Since(mediumReceivedAt) > finalTimeout {
				return dedupeGeneratedImages(items), nil
			}
		case "error":
			errCode := asString(msg["err_code"])
			errMsg := asString(msg["err_msg"])
			if errCode == "rate_limit_exceeded" {
				return dedupeGeneratedImages(items), &UpstreamError{Status: http.StatusTooManyRequests, Body: defaultIfEmpty(errMsg, errCode)}
			}
			if errMsg == "" {
				errMsg = errCode
			}
			if errMsg == "" {
				errMsg = "websocket image generation failed"
			}
			return dedupeGeneratedImages(items), fmt.Errorf("%s", errMsg)
		}
	}

	return dedupeGeneratedImages(items), nil
}

func dialImagineWebsocket(ctx context.Context, wsURL string, headers http.Header, timeout time.Duration, cfg *config.Service, proxyURL string) (*gws.Conn, error) {
	dialer := &gws.Dialer{
		HandshakeTimeout:  timeout,
		EnableCompression: false,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.GetBool("proxy.egress.skip_ssl_verify", false), //nolint:gosec
		},
	}

	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(proxyURL))
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(strings.ToLower(parsed.Scheme), "socks") {
			dialContext, err := proxy.DialContext(cfg, proxyURL)
			if err != nil {
				return nil, err
			}
			dialer.NetDialContext = dialContext
		} else {
			dialer.Proxy = http.ProxyURL(parsed)
		}
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return nil, &UpstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		}
		return nil, err
	}
	return conn, nil
}

func classifyWSImage(msg map[string]any, finalMinBytes, mediumMinBytes int) *GeneratedImage {
	rawURL := absolutizeAssetURL(asString(msg["url"]))
	blob := asString(msg["blob"])
	if rawURL == "" || blob == "" {
		return nil
	}
	imageID := imageIDFromWSURL(rawURL)
	if imageID == "" {
		imageID = stableImageID(rawURL, blob)
	}
	size := len(blob)
	stage := "preview"
	isFinal := false
	switch {
	case size >= finalMinBytes:
		stage = "final"
		isFinal = true
	case size > mediumMinBytes:
		stage = "medium"
	}
	return &GeneratedImage{
		URL:      rawURL,
		BlobB64:  blob,
		Progress: progressForStage(stage),
		ImageID:  imageID,
		Stage:    stage,
		IsFinal:  isFinal,
	}
}

func imageIDFromWSURL(rawURL string) string {
	match := imagePathIDRe.FindStringSubmatch(rawURL)
	if len(match) == 3 {
		return match[1]
	}
	return ""
}

func stableImageID(rawURL, blob string) string {
	hash := sha1.Sum([]byte(rawURL + ":" + blob))
	return fmt.Sprintf("%x", hash[:8])
}

func progressForStage(stage string) int {
	switch stage {
	case "final":
		return 100
	case "medium":
		return 75
	default:
		return 35
	}
}

func collectImageCandidatesFromFrames(frames []map[string]any) ([]GeneratedImage, string) {
	out := make([]GeneratedImage, 0, len(frames))
	reasoning := []string{}
	for _, frame := range frames {
		root, _ := frame["result"].(map[string]any)
		response, _ := root["response"].(map[string]any)
		if response != nil {
			if thinking, _ := response["isThinking"].(bool); thinking {
				if tokenValue, _ := response["token"].(string); tokenValue != "" {
					reasoning = append(reasoning, tokenValue)
				}
			}
			if tokenValue, _ := response["token"].(string); tokenValue != "" {
				for _, raw := range uniqueURLs(imageURLPattern.FindAllString(tokenValue, -1)) {
					out = append(out, GeneratedImage{URL: raw, Progress: 100, Stage: "final", IsFinal: true})
				}
			}
		}
		out = append(out, extractFrameGeneratedImages(frame)...)
	}
	return dedupeGeneratedImages(out), strings.Join(reasoning, "\n")
}

func extractFrameGeneratedImages(frame map[string]any) []GeneratedImage {
	out := make([]GeneratedImage, 0, 4)
	out = append(out, extractCardChunkImages(frame)...)
	out = append(out, extractStreamingGeneratedImages(frame)...)
	for _, raw := range extractGeneratedImageURLs(frame) {
		out = append(out, GeneratedImage{
			URL:      absolutizeAssetURL(raw),
			Progress: 100,
			Stage:    "final",
			IsFinal:  true,
		})
	}
	return dedupeGeneratedImages(out)
}

func extractCardChunkImages(frame map[string]any) []GeneratedImage {
	root, _ := frame["result"].(map[string]any)
	response, _ := root["response"].(map[string]any)
	if response == nil {
		return nil
	}
	cardRaw, _ := response["cardAttachment"].(map[string]any)
	if cardRaw == nil {
		return nil
	}
	jsonData, _ := cardRaw["jsonData"].(string)
	if strings.TrimSpace(jsonData) == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonData), &payload); err != nil {
		return nil
	}
	chunk, _ := payload["image_chunk"].(map[string]any)
	if chunk == nil {
		return nil
	}
	if moderated, _ := chunk["moderated"].(bool); moderated {
		return nil
	}
	imageURL := absolutizeAssetURL(asString(chunk["imageUrl"]))
	if imageURL == "" {
		return nil
	}
	progress, _ := asInt(chunk["progress"])
	stage := stageFromProgress(progress)
	return []GeneratedImage{{
		URL:      imageURL,
		Progress: progress,
		ImageID:  strings.TrimSpace(asString(chunk["imageUuid"])),
		Stage:    stage,
		IsFinal:  stage == "final",
	}}
}

func extractStreamingGeneratedImages(frame map[string]any) []GeneratedImage {
	root, _ := frame["result"].(map[string]any)
	response, _ := root["response"].(map[string]any)
	if response == nil {
		return nil
	}
	streamResp, _ := response["streamingImageGenerationResponse"].(map[string]any)
	if streamResp == nil {
		return nil
	}
	imageURL := absolutizeAssetURL(asString(streamResp["url"]))
	if imageURL == "" {
		imageURL = absolutizeAssetURL(asString(streamResp["imageUrl"]))
	}
	if imageURL == "" {
		return nil
	}
	progress, _ := asInt(streamResp["progress"])
	stage := stageFromProgress(progress)
	imageID := strings.TrimSpace(asString(streamResp["imageId"]))
	if imageID == "" {
		imageID = strings.TrimSpace(asString(streamResp["imageUuid"]))
	}
	return []GeneratedImage{{
		URL:      imageURL,
		Progress: progress,
		ImageID:  imageID,
		Stage:    stage,
		IsFinal:  stage == "final",
	}}
}

func selectFinalOrPartialImages(items []GeneratedImage, count int) ([]GeneratedImage, string) {
	items = dedupeGeneratedImages(items)
	if count <= 0 {
		count = 1
	}
	finals := make([]GeneratedImage, 0, count)
	partials := make([]GeneratedImage, 0, count)
	for _, item := range items {
		if item.URL == "" && item.BlobB64 == "" {
			continue
		}
		if item.IsFinal || item.Stage == "final" {
			finals = append(finals, item)
			continue
		}
		partials = append(partials, item)
	}
	if len(finals) > 0 {
		if len(finals) > count {
			finals = finals[:count]
		}
		if len(finals) < count {
			sortGeneratedImages(partials)
			for _, item := range partials {
				if len(finals) >= count {
					break
				}
				finals = append(finals, item)
			}
		}
		return finals, selectedStage(finals)
	}
	sortGeneratedImages(partials)
	if len(partials) > count {
		partials = partials[:count]
	}
	return partials, selectedStage(partials)
}

func selectedStage(items []GeneratedImage) string {
	stage := ""
	for _, item := range items {
		if stageRank(item.Stage) > stageRank(stage) {
			stage = item.Stage
		}
	}
	return defaultIfEmpty(stage, "preview")
}

func sortGeneratedImages(items []GeneratedImage) {
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i]
		right := items[j]
		if stageRank(left.Stage) == stageRank(right.Stage) {
			return left.Progress > right.Progress
		}
		return stageRank(left.Stage) > stageRank(right.Stage)
	})
}

func hasEnoughFinals(items []GeneratedImage, count int) bool {
	total := 0
	for _, item := range dedupeGeneratedImages(items) {
		if item.IsFinal || item.Stage == "final" {
			total++
		}
	}
	return total >= count
}

func stageFromProgress(progress int) string {
	switch {
	case progress >= 100:
		return "final"
	case progress >= 50:
		return "medium"
	default:
		return "preview"
	}
}

func stageRank(stage string) int {
	switch stage {
	case "final":
		return 3
	case "medium":
		return 2
	case "preview":
		return 1
	default:
		return 0
	}
}

func resolveImageAspectRatio(size string) string {
	switch strings.TrimSpace(size) {
	case "1280x720":
		return "16:9"
	case "720x1280":
		return "9:16"
	case "1792x1024":
		return "3:2"
	case "1024x1024":
		return "1:1"
	default:
		return "2:3"
	}
}

func isRateLimitedError(err error) bool {
	upstream, ok := err.(*UpstreamError)
	return ok && upstream.Status == http.StatusTooManyRequests
}

func dedupeGeneratedImages(items []GeneratedImage) []GeneratedImage {
	out := make([]GeneratedImage, 0, len(items))
	seen := map[string]GeneratedImage{}
	for _, item := range items {
		key := strings.TrimSpace(item.ImageID)
		if key == "" {
			key = strings.TrimSpace(item.URL)
		}
		if key == "" {
			continue
		}
		if existing, ok := seen[key]; ok {
			if stageRank(item.Stage) > stageRank(existing.Stage) || (stageRank(item.Stage) == stageRank(existing.Stage) && item.Progress > existing.Progress) {
				seen[key] = item
			}
			continue
		}
		seen[key] = item
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, seen[key])
	}
	return out
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprintf("%.0f", v)
	case float32:
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	default:
		return ""
	}
}

func asInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func defaultIfEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func maxIntLocal(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *Client) EditImages(ctx context.Context, token string, req ImageEditRequest) ([]GeneratedImage, error) {
	refs := make([]string, 0, len(req.Inputs))
	for _, item := range req.Inputs {
		asset, err := c.UploadFromInput(ctx, token, item)
		if err != nil {
			return nil, err
		}
		if asset.URL != "" {
			refs = append(refs, asset.URL)
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("image edit requires at least one image input")
	}
	post, err := c.CreateMediaPost(ctx, token, "MEDIA_POST_TYPE_IMAGE", "", req.Prompt, "https://grok.com/imagine")
	if err != nil {
		return nil, err
	}
	postData, _ := post["post"].(map[string]any)
	postID, _ := post["postId"].(string)
	if postID == "" && postData != nil {
		postID, _ = postData["id"].(string)
	}
	if postID == "" {
		postID, _ = post["id"].(string)
	}
	if postID == "" {
		return nil, fmt.Errorf("image edit create-post returned no post id")
	}
	payload := buildImageEditPayload(req.Prompt, refs, postID, c.cfg.GetBool("features.temporary", true), c.cfg.GetBool("features.memory", false))
	frames, err := c.streamJSON(ctx, token, payload)
	if err != nil {
		return nil, err
	}
	finalURLs := map[int]string{}
	nextFallbackIndex := 0
	for _, frame := range frames {
		if stream := extractStreamingEditResponse(frame); stream != nil {
			progress, _ := asInt(stream["progress"])
			if progress >= 100 {
				index, ok := parseImageIndex(stream["imageIndex"])
				if !ok {
					index = nextFallbackIndex
				}
				resolved := resolveEditFinalURL(c.cfg, token, asString(stream["imageUrl"]), asString(stream["assetId"]))
				if resolved != "" {
					finalURLs[index] = resolved
				}
			}
		}
		for _, attachment := range extractModelResponseFileAttachments(frame) {
			resolved := resolveEditFinalURL(c.cfg, token, "", attachment)
			if resolved == "" {
				continue
			}
			index := nextFallbackIndex
			if _, exists := finalURLs[index]; !exists {
				finalURLs[index] = resolved
				nextFallbackIndex++
			}
		}
		for _, raw := range extractGeneratedImageURLs(frame) {
			resolved := absolutizeAssetURL(raw)
			if resolved == "" {
				continue
			}
			index := nextFallbackIndex
			if _, exists := finalURLs[index]; !exists {
				finalURLs[index] = resolved
				nextFallbackIndex++
			}
		}
	}
	keys := make([]int, 0, len(finalURLs))
	for key := range finalURLs {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	out := make([]GeneratedImage, 0, len(keys))
	for _, key := range keys {
		out = append(out, GeneratedImage{URL: finalURLs[key], Progress: 100, ImageID: fmt.Sprintf("edit-%d", key)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("image edit returned no images")
	}
	return out, nil
}

func resolveVideoAspectRatio(size string) string {
	switch strings.TrimSpace(size) {
	case "1280x720", "1792x1024":
		return "16:9"
	case "1024x1024":
		return "1:1"
	default:
		return "9:16"
	}
}

func defaultResolutionName(size string) string {
	if strings.TrimSpace(size) == "" {
		return "720p"
	}
	return "720p"
}

func (c *Client) CreateVideo(ctx context.Context, token string, req VideoRequest) (*VideoResult, error) {
	ctx, cancel := c.withConfigTimeout(ctx, "video.timeout", 60)
	defer cancel()
	preset := strings.TrimSpace(req.Preset)
	if preset == "" {
		preset = "custom"
	}
	resolutionName := strings.TrimSpace(req.ResolutionName)
	if resolutionName == "" {
		resolutionName = defaultResolutionName(req.Size)
	}
	parentPost, err := c.CreateMediaPost(ctx, token, "MEDIA_POST_TYPE_VIDEO", "", req.Prompt, "https://grok.com/imagine")
	if err != nil {
		return nil, err
	}
	parentPostID, _ := parentPost["postId"].(string)
	if parentPostID == "" {
		parentPostID, _ = parentPost["id"].(string)
	}
	refs := make([]string, 0, len(req.InputReferences))
	for _, item := range req.InputReferences {
		asset, uploadErr := c.UploadFromInput(ctx, token, item)
		if uploadErr != nil {
			return nil, uploadErr
		}
		if asset.URL != "" {
			refs = append(refs, asset.URL)
		}
	}
	payload := buildVideoPayload(req.Prompt, parentPostID, resolveVideoAspectRatio(req.Size), resolutionName, preset, req.Seconds, refs)
	frames, err := c.streamJSON(ctx, token, payload)
	if err != nil {
		return nil, err
	}
	result := &VideoResult{Progress: 0, PostID: parentPostID}
	for _, frame := range frames {
		parsed := extractGeneratedVideo(frame)
		if parsed.Progress > result.Progress {
			result.Progress = parsed.Progress
		}
		if parsed.URL != "" {
			result.URL = parsed.URL
		}
		if parsed.AssetID != "" {
			result.AssetID = parsed.AssetID
		}
		if parsed.Thumbnail != "" {
			result.Thumbnail = parsed.Thumbnail
		}
	}
	if result.URL == "" && result.AssetID != "" {
		result.URL = fmt.Sprintf("https://assets.grok.com/%s/content", strings.TrimLeft(result.AssetID, "/"))
	}
	if result.URL == "" {
		link, linkErr := c.CreateMediaLink(ctx, token, parentPostID)
		if linkErr == nil {
			if raw, _ := link["url"].(string); raw != "" {
				result.URL = raw
			}
		}
	}
	if result.URL == "" {
		return nil, fmt.Errorf("video generation returned no video url")
	}
	if result.Progress == 0 {
		result.Progress = 100
	}
	return result, nil
}

func cacheFileID(raw string) string {
	sum := sha1.Sum([]byte(raw))
	return fmt.Sprintf("%x", sum[:16])
}

func scanSSELines(reader io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
	out := []string{}
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}
	return out, scanner.Err()
}
