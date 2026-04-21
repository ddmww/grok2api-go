package openai

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/model"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
)

type imageConfig struct {
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

type videoConfig struct {
	Seconds        int    `json:"seconds"`
	Size           string `json:"size"`
	ResolutionName string `json:"resolution_name"`
	Preset         string `json:"preset"`
}

type preparedImageOutput struct {
	APIURL    string
	ChatValue string
	B64JSON   string
}

type videoJob struct {
	ID          string
	Model       string
	Prompt      string
	Seconds     string
	Size        string
	Status      string
	Progress    int
	CreatedAt   int64
	CompletedAt int64
	VideoURL    string
	ContentPath string
	Error       map[string]any
}

var (
	videoJobsMu sync.RWMutex
	videoJobs   = map[string]*videoJob{}
)

func normalizeImageConfig(cfg imageConfig, modelName string, edit bool) (imageConfig, error) {
	if cfg.N <= 0 {
		cfg.N = 1
	}
	if cfg.Size == "" {
		cfg.Size = "1024x1024"
	}
	if cfg.ResponseFormat == "" {
		cfg.ResponseFormat = "url"
	}
	cfg.ResponseFormat = strings.ToLower(strings.TrimSpace(cfg.ResponseFormat))
	switch cfg.ResponseFormat {
	case "url", "b64_json":
	default:
		return cfg, fmt.Errorf("response_format must be one of ['url', 'b64_json']")
	}
	if edit {
		if cfg.N < 1 || cfg.N > 2 {
			return cfg, fmt.Errorf("n must be between 1 and 2 for image edit")
		}
	} else {
		maxN := 10
		if modelName == "grok-imagine-image-lite" {
			maxN = 4
		}
		if cfg.N < 1 || cfg.N > maxN {
			return cfg, fmt.Errorf("n must be between 1 and %d for model %q", maxN, modelName)
		}
	}
	return cfg, nil
}

func normalizeVideoConfig(cfg videoConfig) (videoConfig, error) {
	if cfg.Seconds == 0 {
		cfg.Seconds = 6
	}
	if cfg.Size == "" {
		cfg.Size = "720x1280"
	}
	switch cfg.Seconds {
	case 6, 10, 12, 16, 20:
	default:
		return cfg, fmt.Errorf("seconds must be one of [6, 10, 12, 16, 20]")
	}
	switch cfg.Size {
	case "720x1280", "1280x720", "1024x1024", "1024x1792", "1792x1024":
	default:
		return cfg, fmt.Errorf("size must be one of [720x1280, 1280x720, 1024x1024, 1024x1792, 1792x1024]")
	}
	return cfg, nil
}

func appURL(state *app.State) string {
	return strings.TrimRight(state.Config.GetString("app.app_url", ""), "/")
}

func localImageURL(state *app.State, id string) string {
	return appURL(state) + "/v1/files/image?id=" + url.QueryEscape(id)
}

func localVideoURL(state *app.State, id string) string {
	return appURL(state) + "/v1/files/video?id=" + url.QueryEscape(id)
}

func fileIDFromURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err == nil {
		parts := strings.Split(parsed.Path, "/")
		for index := len(parts) - 1; index >= 0; index-- {
			part := strings.TrimSpace(parts[index])
			if part == "" {
				continue
			}
			stem := strings.SplitN(part, ".", 2)[0]
			if stem != "" && stem != "image" && stem != "original" && stem != "thumbnail" {
				return stem
			}
		}
	}
	sum := sha1.Sum([]byte(raw))
	return fmt.Sprintf("%x", sum[:16])
}

func saveBytes(dir, fileID, extension string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fileID+extension)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func inferImageExtension(contentType, rawURL string) string {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	}
	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, ".png"):
		return ".png"
	case strings.Contains(lower, ".webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

func inferVideoExtension(contentType, rawURL string) string {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.Contains(contentType, "webm"):
		return ".webm"
	case strings.Contains(contentType, "quicktime"):
		return ".mov"
	}
	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, ".webm"):
		return ".webm"
	case strings.Contains(lower, ".mov"):
		return ".mov"
	default:
		return ".mp4"
	}
}

func normalizeConfiguredImageFormat(state *app.State) string {
	value := strings.ToLower(strings.TrimSpace(state.Config.GetString("features.image_format", "grok_url")))
	switch value {
	case "grok_url", "local_url", "grok_md", "local_md", "base64":
		return value
	default:
		return "grok_url"
	}
}

func normalizeConfiguredVideoFormat(state *app.State) string {
	value := strings.ToLower(strings.TrimSpace(state.Config.GetString("features.video_format", "grok_url")))
	switch value {
	case "grok_url", "local_url", "grok_html", "local_html":
		return value
	default:
		return "grok_url"
	}
}

func markdownImage(urlValue string) string {
	return fmt.Sprintf("![image](%s)", urlValue)
}

func htmlVideo(urlValue string) string {
	safe := strings.ReplaceAll(urlValue, `"`, "&quot;")
	return fmt.Sprintf(`<video controls src="%s"></video>`, safe)
}

func inferImageContentType(rawURL string) string {
	extension := inferImageExtension("", rawURL)
	if contentType := mime.TypeByExtension(extension); contentType != "" {
		return contentType
	}
	return "image/jpeg"
}

func ensureLocalImageURL(ctx context.Context, state *app.State, token string, item xai.GeneratedImage) (string, error) {
	if item.URL == "" {
		return "", fmt.Errorf("empty image url")
	}
	fileID := fileIDFromURL(item.URL)
	path, _ := localFilePath(paths.ImageCacheDir(), fileID)
	if path != "" {
		return localImageURL(state, fileID), nil
	}
	data, contentType, err := state.XAI.DownloadContent(ctx, token, item.URL)
	if err != nil {
		return "", err
	}
	extension := inferImageExtension(contentType, item.URL)
	if _, err := saveBytes(paths.ImageCacheDir(), fileID, extension, data); err != nil {
		return "", err
	}
	return localImageURL(state, fileID), nil
}

func imageDataURI(ctx context.Context, state *app.State, token string, item xai.GeneratedImage) (string, string, error) {
	contentType := inferImageContentType(item.URL)
	if strings.TrimSpace(item.BlobB64) != "" {
		return item.BlobB64, contentType, nil
	}
	data, actualType, err := state.XAI.DownloadContent(ctx, token, item.URL)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(actualType) != "" {
		contentType = actualType
	}
	return base64.StdEncoding.EncodeToString(data), contentType, nil
}

func prepareImageOutput(ctx context.Context, state *app.State, token string, item xai.GeneratedImage, responseFormat string) (preparedImageOutput, error) {
	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "b64_json" {
		b64, _, err := imageDataURI(ctx, state, token, item)
		if err != nil {
			return preparedImageOutput{}, err
		}
		return preparedImageOutput{B64JSON: b64}, nil
	}

	configured := normalizeConfiguredImageFormat(state)
	upstreamURL := item.URL
	localURL := ""
	getLocal := func() string {
		if localURL != "" {
			return localURL
		}
		urlValue, err := ensureLocalImageURL(ctx, state, token, item)
		if err == nil {
			localURL = urlValue
		}
		return localURL
	}

	switch configured {
	case "local_url":
		if urlValue := getLocal(); urlValue != "" {
			return preparedImageOutput{APIURL: urlValue, ChatValue: urlValue}, nil
		}
		return preparedImageOutput{APIURL: upstreamURL, ChatValue: upstreamURL}, nil
	case "grok_md":
		return preparedImageOutput{APIURL: upstreamURL, ChatValue: markdownImage(upstreamURL)}, nil
	case "local_md":
		if urlValue := getLocal(); urlValue != "" {
			return preparedImageOutput{APIURL: urlValue, ChatValue: markdownImage(urlValue)}, nil
		}
		return preparedImageOutput{APIURL: upstreamURL, ChatValue: markdownImage(upstreamURL)}, nil
	case "base64":
		b64, contentType, err := imageDataURI(ctx, state, token, item)
		if err != nil {
			return preparedImageOutput{APIURL: upstreamURL, ChatValue: markdownImage(upstreamURL)}, nil
		}
		dataURI := "data:" + contentType + ";base64," + b64
		return preparedImageOutput{APIURL: dataURI, ChatValue: markdownImage(dataURI)}, nil
	default:
		return preparedImageOutput{APIURL: upstreamURL, ChatValue: upstreamURL}, nil
	}
}

func ensureLocalVideoURL(ctx context.Context, state *app.State, token, rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	fileID := fileIDFromURL(rawURL)
	path, _ := localFilePath(paths.VideoCacheDir(), fileID)
	if path != "" {
		return localVideoURL(state, fileID)
	}
	data, contentType, err := state.XAI.DownloadContent(ctx, token, rawURL)
	if err != nil {
		return ""
	}
	extension := inferVideoExtension(contentType, rawURL)
	if _, err := saveBytes(paths.VideoCacheDir(), fileID, extension, data); err != nil {
		return ""
	}
	return localVideoURL(state, fileID)
}

func resolveVideoLink(ctx context.Context, state *app.State, token, rawURL string) string {
	configured := normalizeConfiguredVideoFormat(state)
	switch configured {
	case "local_url", "local_html":
		if urlValue := ensureLocalVideoURL(ctx, state, token, rawURL); urlValue != "" {
			return urlValue
		}
	}
	return rawURL
}

func renderVideoValue(state *app.State, rawURL string) string {
	configured := normalizeConfiguredVideoFormat(state)
	switch configured {
	case "grok_html", "local_html":
		return htmlVideo(rawURL)
	default:
		return rawURL
	}
}

func imageOutputs(ctx context.Context, state *app.State, token string, items []xai.GeneratedImage, responseFormat string) ([]map[string]any, error) {
	outputs := make([]map[string]any, 0, len(items))
	for _, item := range items {
		output, err := prepareImageOutput(ctx, state, token, item, responseFormat)
		if err != nil {
			return nil, err
		}
		if output.B64JSON != "" {
			outputs = append(outputs, map[string]any{"b64_json": output.B64JSON})
			continue
		}
		outputs = append(outputs, map[string]any{"url": output.APIURL})
	}
	return outputs, nil
}

func extractPromptFromMessages(messages []map[string]any) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if role, _ := messages[index]["role"].(string); role == "user" {
			text := stringifyContent(messages[index]["content"])
			if strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

func extractImageInputs(messages []map[string]any) []string {
	inputs := []string{}
	for _, message := range messages {
		content, ok := message["content"].([]map[string]any)
		if ok {
			for _, item := range content {
				if raw := extractImageInputURL(item); raw != "" {
					inputs = append(inputs, raw)
				}
			}
			continue
		}
		generic, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, entry := range generic {
			mapped, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if raw := extractImageInputURL(mapped); raw != "" {
				inputs = append(inputs, raw)
			}
		}
	}
	return inputs
}

func extractImageInputURL(item map[string]any) string {
	if item == nil {
		return ""
	}
	if itemType, _ := item["type"].(string); itemType != "image_url" && itemType != "file" {
		return ""
	}
	if imageURL, ok := item["image_url"].(map[string]any); ok {
		if raw, _ := imageURL["url"].(string); raw != "" {
			return raw
		}
	}
	if imageURL, ok := item["image_url"].(string); ok && imageURL != "" {
		return imageURL
	}
	if file, ok := item["file"].(map[string]any); ok {
		if raw, _ := file["data"].(string); raw != "" {
			return raw
		}
	}
	return ""
}

func generateImages(ctx context.Context, state *app.State, spec model.Spec, prompt string, cfg imageConfig, chatFormat bool) (any, error) {
	retryCodes := parseRetryCodes(state.Config.GetString("retry.on_codes", "429,503"))
	maxRetries := maxInt(state.Config.GetInt("retry.max_retries", 1), 0)
	excluded := map[string]struct{}{}
	var lastRetryErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		lease, err := reserveLease(state, spec, excluded)
		if err != nil {
			if lastRetryErr != nil {
				return nil, lastRetryErr
			}
			return nil, err
		}
		items, reasoning, meta, err := state.XAI.GenerateImages(ctx, lease.Token, xai.ImageRequest{
			Model:  spec.Name,
			Mode:   spec.Mode,
			Prompt: prompt,
			N:      cfg.N,
			Size:   cfg.Size,
		})
		if err != nil {
			_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
			if shouldRetry(err, retryCodes, attempt, maxRetries) {
				excluded[lease.Token] = struct{}{}
				lastRetryErr = err
				continue
			}
			return nil, err
		}
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, imageFeedback(meta))
		if meta == nil || !meta.SawRateLimit {
			refreshQuotaAsync(state, lease.Token)
		}
		outputs, err := imageOutputs(ctx, state, lease.Token, items, cfg.ResponseFormat)
		if err != nil {
			return nil, err
		}
		if cfg.N < len(outputs) {
			outputs = outputs[:cfg.N]
		}
		if chatFormat {
			contentParts := make([]string, 0, len(outputs))
			for _, item := range items {
				output, err := prepareImageOutput(ctx, state, lease.Token, item, cfg.ResponseFormat)
				if err != nil {
					return nil, err
				}
				if output.ChatValue != "" {
					contentParts = append(contentParts, output.ChatValue)
					continue
				}
				if output.B64JSON != "" {
					contentParts = append(contentParts, output.B64JSON)
				}
			}
			return chatResponse(spec.Name, strings.Join(contentParts, "\n\n"), reasoning, nil, nil), nil
		}
		return map[string]any{"created": time.Now().Unix(), "data": outputs}, nil
	}
	if lastRetryErr != nil {
		return nil, lastRetryErr
	}
	return nil, fmt.Errorf("no available accounts")
}

func imageFeedback(meta *xai.ImageGenerationMeta) account.Feedback {
	if meta != nil && meta.SawRateLimit {
		return account.Feedback{Kind: account.FeedbackRateLimited, Reason: "image rate limit exceeded"}
	}
	return account.Feedback{Kind: account.FeedbackSuccess}
}

func editImages(ctx context.Context, state *app.State, spec model.Spec, messages []map[string]any, cfg imageConfig, chatFormat bool) (any, error) {
	lease, err := reserveLease(state, spec, nil)
	if err != nil {
		return nil, err
	}
	items, err := state.XAI.EditImages(ctx, lease.Token, xai.ImageEditRequest{
		Prompt: extractPromptFromMessages(messages),
		Inputs: extractImageInputs(messages),
		Size:   cfg.Size,
	})
	if err != nil {
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
		return nil, err
	}
	_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
	refreshQuotaAsync(state, lease.Token)
	outputs, err := imageOutputs(ctx, state, lease.Token, items, cfg.ResponseFormat)
	if err != nil {
		return nil, err
	}
	if cfg.N < len(outputs) {
		outputs = outputs[:cfg.N]
	}
	if chatFormat {
		contentParts := make([]string, 0, len(outputs))
		for _, item := range items {
			output, err := prepareImageOutput(ctx, state, lease.Token, item, cfg.ResponseFormat)
			if err != nil {
				return nil, err
			}
			if output.ChatValue != "" {
				contentParts = append(contentParts, output.ChatValue)
				continue
			}
			if output.B64JSON != "" {
				contentParts = append(contentParts, output.B64JSON)
			}
		}
		return chatResponse(spec.Name, strings.Join(contentParts, "\n\n"), "", nil, nil), nil
	}
	return map[string]any{"created": time.Now().Unix(), "data": outputs}, nil
}

func createVideo(ctx context.Context, state *app.State, spec model.Spec, prompt string, cfg videoConfig) (map[string]any, error) {
	lease, err := reserveLease(state, spec, nil)
	if err != nil {
		return nil, err
	}
	video, err := state.XAI.CreateVideo(ctx, lease.Token, xai.VideoRequest{
		Model:          spec.Name,
		Prompt:         prompt,
		Seconds:        cfg.Seconds,
		Size:           cfg.Size,
		ResolutionName: cfg.ResolutionName,
		Preset:         cfg.Preset,
	})
	if err != nil {
		_ = state.Runtime.ApplyFeedback(context.Background(), lease, feedbackForError(err))
		return nil, err
	}
	_ = state.Runtime.ApplyFeedback(context.Background(), lease, account.Feedback{Kind: account.FeedbackSuccess})
	refreshQuotaAsync(state, lease.Token)
	job := &videoJob{
		ID:        responseID("video"),
		Model:     spec.Name,
		Prompt:    prompt,
		Seconds:   fmt.Sprintf("%d", cfg.Seconds),
		Size:      cfg.Size,
		Status:    "completed",
		Progress:  maxInt(video.Progress, 100),
		CreatedAt: time.Now().Unix(),
		VideoURL:  video.URL,
	}
	resolvedVideoURL := video.URL
	if localURL := ensureLocalVideoURL(ctx, state, lease.Token, video.URL); localURL != "" {
		resolvedVideoURL = localURL
		if path, _ := localFilePath(paths.VideoCacheDir(), fileIDFromURL(video.URL)); path != "" {
			job.ContentPath = path
		}
	}
	if normalizeConfiguredVideoFormat(state) == "grok_url" || normalizeConfiguredVideoFormat(state) == "grok_html" {
		resolvedVideoURL = video.URL
	}
	job.VideoURL = resolvedVideoURL
	job.CompletedAt = time.Now().Unix()
	videoJobsMu.Lock()
	videoJobs[job.ID] = job
	videoJobsMu.Unlock()
	return job.toMap(), nil
}

func (j *videoJob) toMap() map[string]any {
	payload := map[string]any{
		"id":         j.ID,
		"object":     "video",
		"created_at": j.CreatedAt,
		"status":     j.Status,
		"model":      j.Model,
		"progress":   j.Progress,
		"prompt":     j.Prompt,
		"seconds":    j.Seconds,
		"size":       j.Size,
		"quality":    "standard",
	}
	if j.CompletedAt > 0 {
		payload["completed_at"] = j.CompletedAt
	}
	if j.Error != nil {
		payload["error"] = j.Error
	}
	if j.VideoURL != "" {
		payload["url"] = j.VideoURL
	}
	return payload
}

func getVideoJob(id string) *videoJob {
	videoJobsMu.RLock()
	defer videoJobsMu.RUnlock()
	job := videoJobs[id]
	if job == nil {
		return nil
	}
	copyJob := *job
	return &copyJob
}

func videoChatResponse(state *app.State, spec model.Spec, raw map[string]any) map[string]any {
	if rawURL, _ := raw["url"].(string); rawURL != "" {
		return chatResponse(spec.Name, renderVideoValue(state, rawURL), "", nil, nil)
	}
	b, _ := json.Marshal(raw)
	return chatResponse(spec.Name, string(b), "", nil, nil)
}

func localFilePath(dir, id string) (string, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), id) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		contentType := mime.TypeByExtension(extension)
		if contentType == "" {
			if dir == paths.ImageCacheDir() {
				contentType = "image/jpeg"
			} else {
				contentType = "video/mp4"
			}
		}
		return path, contentType
	}
	return "", ""
}
