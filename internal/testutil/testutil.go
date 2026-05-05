package testutil

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/ddmww/grok2api-go/internal/platform/config"
)

type MemoryBackend struct {
	mu     sync.Mutex
	values map[string]any
}

func NewMemoryBackend(values map[string]any) *MemoryBackend {
	return &MemoryBackend{values: cloneMap(values)}
}

func (b *MemoryBackend) Load(context.Context) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneMap(b.values), nil
}

func (b *MemoryBackend) Save(_ context.Context, values map[string]any) error {
	b.mu.Lock()
	b.values = cloneMap(values)
	b.mu.Unlock()
	return nil
}

func NewConfig(values map[string]any) *config.Service {
	backend := NewMemoryBackend(values)
	service := config.New(backend)
	_ = service.Load(context.Background())
	return service
}

type CloseNotifyRecorder struct {
	*httptest.ResponseRecorder
	notifyCh chan bool
}

func NewCloseNotifyRecorder() *CloseNotifyRecorder {
	return &CloseNotifyRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		notifyCh:         make(chan bool, 1),
	}
}

func (r *CloseNotifyRecorder) CloseNotify() <-chan bool {
	return r.notifyCh
}

type FakeGrokServer struct {
	Server              *httptest.Server
	mu                  sync.Mutex
	ChatContent         string
	Reasoning           string
	Assets              []map[string]any
	ImageURL            string
	PartialImageURL     string
	PreviewImageURL     string
	VideoURL            string
	AppChatImageMode    string
	WebsocketImageMode  string
	ImageEditMode       string
	ImageDownloadMode   string
	RateLimitMode       string
	AppChatImageModes   map[string]string
	WebsocketImageModes map[string]string
	RateLimitCalls      map[string]int
}

func NewFakeGrokServer() *FakeGrokServer {
	fake := &FakeGrokServer{
		ChatContent:         "Hello from fake grok",
		Reasoning:           "thinking",
		Assets:              []map[string]any{{"id": "asset-1"}, {"id": "asset-2"}},
		AppChatImageMode:    "final",
		WebsocketImageMode:  "final",
		ImageEditMode:       "final",
		AppChatImageModes:   map[string]string{},
		WebsocketImageModes: map[string]string{},
		RateLimitCalls:      map[string]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/app-chat/conversations/new", fake.handleChat)
	mux.HandleFunc("/rest/rate-limits", fake.handleRateLimits)
	mux.HandleFunc("/rest/auth/set-birth-date", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/auth_mgmt.AuthManagement/UpdateUserFeatureControls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/auth_mgmt.AuthManagement/SetTosAcceptedVersion", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/rest/assets", fake.handleAssets)
	mux.HandleFunc("/rest/assets-metadata/", fake.handleDeleteAsset)
	mux.HandleFunc("/rest/app-chat/upload-file", fake.handleUpload)
	mux.HandleFunc("/rest/media/post/create", fake.handleMediaPost)
	mux.HandleFunc("/rest/media/post/create-link", fake.handleMediaLink)
	mux.HandleFunc("/generated/image.png", fake.handleImage)
	mux.HandleFunc("/generated/partial.png", fake.handleImage)
	mux.HandleFunc("/generated/preview.png", fake.handleImage)
	mux.HandleFunc("/generated/video.mp4", fake.handleVideo)
	fake.Server = httptest.NewServer(mux)
	fake.ImageURL = fake.Server.URL + "/generated/image.png"
	fake.PartialImageURL = fake.Server.URL + "/generated/partial.png"
	fake.PreviewImageURL = fake.Server.URL + "/generated/preview.png"
	fake.VideoURL = fake.Server.URL + "/generated/video.mp4"
	return fake
}

func (f *FakeGrokServer) Close() {
	if f.Server != nil {
		f.Server.Close()
	}
}

func (f *FakeGrokServer) BaseURL() string {
	if f.Server == nil {
		return ""
	}
	return f.Server.URL
}

func (f *FakeGrokServer) handleChat(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	message, _ := payload["message"].(string)
	modelName, _ := payload["modelName"].(string)
	modeID, _ := payload["modeId"].(string)

	if modelName == "imagine-image-edit" {
		if strings.EqualFold(f.ImageEditMode, "rate_limit") {
			http.Error(w, `{"error":"image rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		progressURL := f.PartialImageURL
		progressValue := 60
		if strings.EqualFold(f.ImageEditMode, "final") {
			progressURL = f.ImageURL
			progressValue = 100
		}
		frame, _ := json.Marshal(map[string]any{
			"result": map[string]any{
				"response": map[string]any{
					"streamingImageGenerationResponse": map[string]any{
						"progress":   progressValue,
						"imageUrl":   progressURL,
						"assetId":    "uploaded-asset-1",
						"imageIndex": 0,
					},
					"modelResponse": map[string]any{
						"fileAttachments": []string{"uploaded-asset-1"},
					},
				},
			},
		})
		done, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"finalMetadata": map[string]any{"complete": true}}}})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_, _ = w.Write([]byte("data: " + string(done) + "\n\n"))
		return
	}

	if strings.HasPrefix(message, "Drawing:") || strings.Contains(strings.ToLower(modelName), "image") {
		f.handleImageChat(w, r, message, modelName, modeID)
		return
	}

	content := f.ChatContent
	if strings.Contains(message, "call_tool") {
		content = "<tool_calls><tool_call><tool_name>lookup_weather</tool_name><parameters>{\"city\":\"Shanghai\"}</parameters></tool_call></tool_calls>"
	} else if strings.Contains(strings.ToLower(message), "video") || strings.Contains(strings.ToLower(modelName), "video") {
		content = f.VideoURL
	} else if strings.Contains(strings.ToLower(message), "image") || strings.Contains(strings.ToLower(message), "edit") || strings.Contains(strings.ToLower(modelName), "image") {
		content = f.ImageURL
	}

	w.Header().Set("Content-Type", "text/event-stream")
	thinking, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"token": f.Reasoning, "isThinking": true}}})
	answerPayload := map[string]any{"token": content, "messageTag": "final"}
	if strings.Contains(message, "model_response_only") {
		answerPayload = map[string]any{"modelResponse": map[string]any{"message": content}}
	}
	answer, _ := json.Marshal(map[string]any{"result": map[string]any{"response": answerPayload}})
	done, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"finalMetadata": map[string]any{"complete": true}}}})
	_, _ = w.Write([]byte("data: " + string(thinking) + "\n\n"))
	_, _ = w.Write([]byte("data: " + string(answer) + "\n\n"))
	_, _ = w.Write([]byte("data: " + string(done) + "\n\n"))
}

func (f *FakeGrokServer) handleImageChat(w http.ResponseWriter, r *http.Request, message, modelName, modeID string) {
	channel := "websocket"
	scenario := f.WebsocketImageMode
	token := extractSSOToken(r.Header.Get("Cookie"))
	if strings.HasPrefix(message, "Drawing:") || modeID != "" {
		channel = "app_chat"
		scenario = f.AppChatImageMode
		if tokenScenario := f.imageModeForToken(token, true); tokenScenario != "" {
			scenario = tokenScenario
		}
	} else if tokenScenario := f.imageModeForToken(token, false); tokenScenario != "" {
		scenario = tokenScenario
	}
	if strings.EqualFold(scenario, "rate_limit") {
		http.Error(w, `{"error":"image rate limit exceeded"}`, http.StatusTooManyRequests)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	thinking, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"token": f.Reasoning, "isThinking": true}}})
	_, _ = w.Write([]byte("data: " + string(thinking) + "\n\n"))

	switch strings.ToLower(strings.TrimSpace(scenario)) {
	case "partial":
		_, _ = w.Write([]byte("data: " + string(f.imageFrame(channel, 60, f.PartialImageURL, false)) + "\n\n"))
	case "preview":
		_, _ = w.Write([]byte("data: " + string(f.imageFrame(channel, 20, f.PreviewImageURL, false)) + "\n\n"))
	case "empty":
	default:
		_, _ = w.Write([]byte("data: " + string(f.imageFrame(channel, 100, f.ImageURL, true)) + "\n\n"))
	}

	done, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"finalMetadata": map[string]any{"complete": true}}}})
	_, _ = w.Write([]byte("data: " + string(done) + "\n\n"))
}

func (f *FakeGrokServer) imageModeForToken(token string, appChat bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if appChat {
		return strings.TrimSpace(f.AppChatImageModes[token])
	}
	return strings.TrimSpace(f.WebsocketImageModes[token])
}

func extractSSOToken(cookieHeader string) string {
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "sso=") {
			return strings.TrimPrefix(part, "sso=")
		}
	}
	return ""
}

func (f *FakeGrokServer) imageFrame(channel string, progress int, imageURL string, final bool) []byte {
	response := map[string]any{}
	imageID := "img-app"
	if channel != "app_chat" {
		imageID = "img-ws"
	}
	if channel == "app_chat" {
		cardData, _ := json.Marshal(map[string]any{
			"image_chunk": map[string]any{
				"imageUuid": imageID,
				"imageUrl":  imageURL,
				"progress":  progress,
			},
		})
		response["cardAttachment"] = map[string]any{"jsonData": string(cardData)}
		if final {
			response["token"] = imageURL
		}
	} else {
		response["streamingImageGenerationResponse"] = map[string]any{
			"imageUuid": imageID,
			"imageUrl":  imageURL,
			"url":       imageURL,
			"progress":  progress,
		}
		if final {
			response["modelResponse"] = map[string]any{"generatedImageUrls": []string{imageURL}}
		}
	}
	frame, _ := json.Marshal(map[string]any{"result": map[string]any{"response": response}})
	return frame
}

func (f *FakeGrokServer) handleRateLimits(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	modelName, _ := payload["modelName"].(string)
	token := extractSSOToken(r.Header.Get("Cookie"))
	key := token + "|" + modelName
	f.mu.Lock()
	f.RateLimitCalls[key]++
	rateLimitMode := f.RateLimitMode
	f.mu.Unlock()
	if strings.EqualFold(rateLimitMode, "rate_limit") {
		http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
		return
	}
	total := 20
	switch modelName {
	case "fast":
		total = 60
	case "expert":
		total = 8
	case "heavy":
		total = 20
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"remainingQueries":  total,
		"totalQueries":      total,
		"windowSizeSeconds": 3600,
	})
}

func (f *FakeGrokServer) RateLimitCallCount(token, mode string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.RateLimitCalls[token+"|"+mode]
}

func (f *FakeGrokServer) handleAssets(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"assets": cloneSlice(f.Assets)})
}

func (f *FakeGrokServer) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/rest/assets-metadata/")
	f.mu.Lock()
	filtered := make([]map[string]any, 0, len(f.Assets))
	for _, asset := range f.Assets {
		if assetID, _ := asset["id"].(string); assetID != id {
			filtered = append(filtered, asset)
		}
	}
	f.Assets = filtered
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakeGrokServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"fileMetadataId": "uploaded-asset-1",
		"fileUri":        "/users/test/uploaded-asset-1/content",
	})
}

func (f *FakeGrokServer) handleMediaPost(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"postId": "post-1",
		"id":     "post-1",
		"url":    f.VideoURL,
	})
}

func (f *FakeGrokServer) handleMediaLink(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"url": f.VideoURL,
	})
}

func (f *FakeGrokServer) handleImage(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(f.ImageDownloadMode, "rate_limit") {
		http.Error(w, `{"error":"image download rate limit exceeded"}`, http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write([]byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0xc9, 0xfe, 0x92,
		0xef, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	})
}

func (f *FakeGrokServer) handleVideo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/mp4")
	_, _ = w.Write([]byte("fake mp4 data"))
}

func cloneMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = cloneMap(typed)
		case []any:
			copied := make([]any, len(typed))
			copy(copied, typed)
			out[key] = copied
		case []map[string]any:
			next := make([]map[string]any, len(typed))
			for index, item := range typed {
				next[index] = cloneMap(item)
			}
			out[key] = next
		default:
			out[key] = value
		}
	}
	return out
}

func cloneSlice(items []map[string]any) []map[string]any {
	out := make([]map[string]any, len(items))
	for index, item := range items {
		out[index] = cloneMap(item)
	}
	return out
}
