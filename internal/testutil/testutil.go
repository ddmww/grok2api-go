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

type FakeGrokServer struct {
	Server      *httptest.Server
	mu          sync.Mutex
	ChatContent string
	Reasoning   string
	Assets      []map[string]any
}

func NewFakeGrokServer() *FakeGrokServer {
	fake := &FakeGrokServer{
		ChatContent: "Hello from fake grok",
		Reasoning:   "thinking",
		Assets:      []map[string]any{{"id": "asset-1"}, {"id": "asset-2"}},
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
	mux.HandleFunc("/rest/assets", fake.handleAssets)
	mux.HandleFunc("/rest/assets-metadata/", fake.handleDeleteAsset)
	fake.Server = httptest.NewServer(mux)
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

	content := f.ChatContent
	if strings.Contains(message, "call_tool") {
		content = "<tool_calls><tool_call><tool_name>lookup_weather</tool_name><parameters>{\"city\":\"Shanghai\"}</parameters></tool_call></tool_calls>"
	}

	w.Header().Set("Content-Type", "text/event-stream")
	thinking, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"token": f.Reasoning, "isThinking": true}}})
	answer, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"token": content, "messageTag": "final"}}})
	done, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"finalMetadata": map[string]any{"complete": true}}}})
	_, _ = w.Write([]byte("data: " + string(thinking) + "\n\n"))
	_, _ = w.Write([]byte("data: " + string(answer) + "\n\n"))
	_, _ = w.Write([]byte("data: " + string(done) + "\n\n"))
}

func (f *FakeGrokServer) handleRateLimits(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	modelName, _ := payload["modelName"].(string)
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
