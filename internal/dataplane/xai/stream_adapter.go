package xai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ddmww/grok2api-go/internal/platform/config"
)

type FrameEvent struct {
	Kind           string
	Content        string
	ImageID        string
	AnnotationData map[string]any
}

type StreamAdapter struct {
	cfg               *config.Service
	cardCache         map[string]map[string]any
	citationOrder     []string
	citationMap       map[string]int
	lastCitationIndex int
	pendingCitations  []pendingCitation
	annotations       []map[string]any
	textOffset        int
	webSearchResults  []map[string]any
	webSearchSeen     map[string]struct{}
	ThinkingBuf       []string
	TextBuf           []string
	ImageURLs         []ImageRef
	FinalMessage      string
	streamErrors      []string
}

type pendingCitation struct {
	URL    string
	Title  string
	Needle string
}

type ImageRef struct {
	URL     string
	ImageID string
}

var (
	grokRenderRe  = regexp.MustCompile(`(?is)<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)
	citationRefRe = regexp.MustCompile(`\s\[\[(\d+)\]\]\(([^)]+)\)`)
)

func NewStreamAdapter(cfg *config.Service) *StreamAdapter {
	return &StreamAdapter{
		cfg:               cfg,
		cardCache:         map[string]map[string]any{},
		citationMap:       map[string]int{},
		lastCitationIndex: -1,
		webSearchSeen:     map[string]struct{}{},
	}
}

func ClassifyLine(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "event:") {
		return "skip", ""
	}
	if strings.HasPrefix(line, "data:") {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return "done", ""
		}
		return "data", data
	}
	if strings.HasPrefix(line, "{") {
		return "data", line
	}
	return "skip", ""
}

func (a *StreamAdapter) Feed(data string) []FrameEvent {
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil
	}
	result, _ := payload["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return nil
	}

	events := []FrameEvent{}
	if cardRaw, _ := response["cardAttachment"].(map[string]any); cardRaw != nil {
		events = append(events, a.handleCard(cardRaw)...)
	}
	if modelResponse, _ := response["modelResponse"].(map[string]any); modelResponse != nil {
		a.captureModelResponse(modelResponse)
	}
	a.captureResponseError(response)
	a.collectSearchSources(response)

	if response["finalMetadata"] != nil {
		return append(events, FrameEvent{Kind: "soft_stop"})
	}
	if isSoftStop, _ := response["isSoftStop"].(bool); isSoftStop {
		return append(events, FrameEvent{Kind: "soft_stop"})
	}

	token, _ := response["token"].(string)
	if token == "" {
		return events
	}
	if isThinking, _ := response["isThinking"].(bool); isThinking {
		a.ThinkingBuf = append(a.ThinkingBuf, token)
		return append(events, FrameEvent{Kind: "thinking", Content: token})
	}

	cleaned, localAnnotations := a.cleanToken(token)
	if cleaned != "" {
		a.TextBuf = append(a.TextBuf, cleaned)
		for _, ann := range localAnnotations {
			start, _ := ann["local_start"].(int)
			end, _ := ann["local_end"].(int)
			delete(ann, "local_start")
			delete(ann, "local_end")
			ann["start_index"] = a.textOffset + start
			ann["end_index"] = a.textOffset + end
			a.annotations = append(a.annotations, ann)
			events = append(events, FrameEvent{Kind: "annotation", AnnotationData: ann})
		}
		a.textOffset += len(cleaned)
		events = append(events, FrameEvent{Kind: "text", Content: cleaned})
	}
	return events
}

func (a *StreamAdapter) captureModelResponse(modelResponse map[string]any) {
	message := strings.TrimSpace(asString(modelResponse["message"]))
	if message == "" {
		return
	}
	cardMap := map[string]map[string]any{}
	if attachments, _ := modelResponse["cardAttachmentsJson"].([]any); attachments != nil {
		for _, raw := range attachments {
			text, _ := raw.(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			var cardData map[string]any
			if err := json.Unmarshal([]byte(text), &cardData); err != nil || cardData == nil {
				continue
			}
			cardID := strings.TrimSpace(asString(cardData["id"]))
			if cardID == "" {
				continue
			}
			cardMap[cardID] = cardData
		}
	}
	message = grokRenderRe.ReplaceAllStringFunc(message, func(raw string) string {
		match := grokRenderRe.FindStringSubmatch(raw)
		if len(match) < 4 {
			return ""
		}
		card := cardMap[match[1]]
		if card == nil {
			return ""
		}
		image, _ := card["image"].(map[string]any)
		original := strings.TrimSpace(asString(image["original"]))
		if original == "" {
			return ""
		}
		title := strings.TrimSpace(asString(image["title"]))
		if title == "" {
			title = "image"
		}
		return fmt.Sprintf("![%s](%s)", title, original)
	})
	a.FinalMessage = message
	if items, ok := modelResponse["streamErrors"].([]any); ok {
		for _, item := range items {
			if mapped, ok := item.(map[string]any); ok {
				a.recordStreamError(asString(mapped["message"]))
			}
		}
	}
}

func (a *StreamAdapter) captureResponseError(response map[string]any) {
	if response == nil {
		return
	}
	if payload, _ := response["error"].(map[string]any); payload != nil {
		a.recordStreamError(asString(payload["message"]))
	}
	if items, ok := response["streamErrors"].([]any); ok {
		for _, item := range items {
			if mapped, ok := item.(map[string]any); ok {
				a.recordStreamError(asString(mapped["message"]))
			}
		}
	}
}

func (a *StreamAdapter) recordStreamError(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	for _, existing := range a.streamErrors {
		if existing == message {
			return
		}
	}
	a.streamErrors = append(a.streamErrors, message)
}

func (a *StreamAdapter) handleCard(cardRaw map[string]any) []FrameEvent {
	jsonData, _ := cardRaw["jsonData"].(string)
	if strings.TrimSpace(jsonData) == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonData), &payload); err != nil {
		return nil
	}
	cardID := strings.TrimSpace(asString(payload["id"]))
	if cardID != "" {
		a.cardCache[cardID] = payload
	}

	chunk, _ := payload["image_chunk"].(map[string]any)
	if chunk == nil {
		return nil
	}
	progress, _ := asInt(chunk["progress"])
	imageID := strings.TrimSpace(asString(chunk["imageUuid"]))
	events := []FrameEvent{}
	if progress > 0 {
		events = append(events, FrameEvent{Kind: "image_progress", Content: fmt.Sprintf("%d", progress), ImageID: imageID})
	}
	if moderated, _ := chunk["moderated"].(bool); moderated {
		return events
	}
	if progress == 100 {
		rawURL := absolutizeAssetURL(asString(chunk["imageUrl"]))
		if rawURL != "" {
			a.ImageURLs = append(a.ImageURLs, ImageRef{URL: rawURL, ImageID: imageID})
			events = append(events, FrameEvent{Kind: "image", Content: rawURL, ImageID: imageID})
		}
	}
	return events
}

func (a *StreamAdapter) collectSearchSources(response map[string]any) {
	if wsr, _ := response["webSearchResults"].(map[string]any); wsr != nil {
		if results, _ := wsr["results"].([]any); results != nil {
			for _, item := range results {
				mapped, _ := item.(map[string]any)
				url := strings.TrimSpace(asString(mapped["url"]))
				if url == "" {
					continue
				}
				if _, ok := a.webSearchSeen[url]; ok {
					continue
				}
				a.webSearchSeen[url] = struct{}{}
				a.webSearchResults = append(a.webSearchResults, map[string]any{
					"url":   url,
					"title": defaultIfEmpty(strings.TrimSpace(asString(mapped["title"])), url),
					"type":  "web",
				})
			}
		}
	}
	if xsr, _ := response["xSearchResults"].(map[string]any); xsr != nil {
		if results, _ := xsr["results"].([]any); results != nil {
			for _, item := range results {
				mapped, _ := item.(map[string]any)
				postID := strings.TrimSpace(asString(mapped["postId"]))
				username := strings.TrimSpace(asString(mapped["username"]))
				if postID == "" || username == "" {
					continue
				}
				url := fmt.Sprintf("https://x.com/%s/status/%s", username, postID)
				if _, ok := a.webSearchSeen[url]; ok {
					continue
				}
				a.webSearchSeen[url] = struct{}{}
				title := strings.TrimSpace(asString(mapped["text"]))
				if title == "" {
					title = "𝕏/@" + username
				} else {
					title = "𝕏/@" + username + ": " + normalizeWhitespace(title)
				}
				a.webSearchResults = append(a.webSearchResults, map[string]any{"url": url, "title": title, "type": "x_post"})
			}
		}
	}
}

func (a *StreamAdapter) cleanToken(token string) (string, []map[string]any) {
	if !strings.Contains(token, "<grok:render") {
		return token, nil
	}
	cleaned := grokRenderRe.ReplaceAllStringFunc(token, func(raw string) string {
		match := grokRenderRe.FindStringSubmatch(raw)
		if len(match) < 4 {
			return ""
		}
		return a.renderReplace(match[1], match[3])
	})
	if strings.HasPrefix(cleaned, "\n") && strings.Contains(cleaned, "[[") {
		cleaned = strings.TrimPrefix(cleaned, "\n")
	}

	localAnnotations := []map[string]any{}
	if len(a.pendingCitations) > 0 {
		searchStart := 0
		for _, cite := range a.pendingCitations {
			pos := strings.Index(cleaned[searchStart:], cite.Needle)
			if pos == -1 {
				continue
			}
			pos += searchStart
			localAnnotations = append(localAnnotations, map[string]any{
				"type":        "url_citation",
				"url":         cite.URL,
				"title":       cite.Title,
				"local_start": pos,
				"local_end":   pos + len(cite.Needle),
			})
			searchStart = pos + len(cite.Needle)
		}
		a.pendingCitations = nil
	}
	return cleaned, localAnnotations
}

func (a *StreamAdapter) renderReplace(cardID, renderType string) string {
	card := a.cardCache[cardID]
	if card == nil {
		return ""
	}
	switch renderType {
	case "render_generated_image":
		return ""
	case "render_searched_image":
		image, _ := card["image"].(map[string]any)
		title := defaultIfEmpty(strings.TrimSpace(asString(image["title"])), "image")
		thumb := strings.TrimSpace(asString(image["thumbnail"]))
		if thumb == "" {
			thumb = strings.TrimSpace(asString(image["original"]))
		}
		link := strings.TrimSpace(asString(image["link"]))
		if thumb == "" {
			return ""
		}
		if link != "" {
			return fmt.Sprintf("[![%s](%s)](%s)", title, thumb, link)
		}
		return fmt.Sprintf("![%s](%s)", title, thumb)
	case "render_inline_citation":
		urlValue := strings.TrimSpace(asString(card["url"]))
		if urlValue == "" {
			return ""
		}
		index, ok := a.citationMap[urlValue]
		if !ok {
			a.citationOrder = append(a.citationOrder, urlValue)
			index = len(a.citationOrder)
			a.citationMap[urlValue] = index
		}
		if index == a.lastCitationIndex {
			return ""
		}
		a.lastCitationIndex = index
		needle := fmt.Sprintf(" [[%d]](%s)", index, urlValue)
		title := strings.TrimSpace(asString(card["title"]))
		if title == "" {
			for _, item := range a.webSearchResults {
				if itemURL, _ := item["url"].(string); itemURL == urlValue {
					title, _ = item["title"].(string)
					break
				}
			}
		}
		if title == "" {
			title = urlValue
		}
		a.pendingCitations = append(a.pendingCitations, pendingCitation{
			URL:    urlValue,
			Title:  title,
			Needle: needle,
		})
		return needle
	default:
		return ""
	}
}

func (a *StreamAdapter) ReferencesSuffix() string {
	if len(a.webSearchResults) == 0 {
		return ""
	}
	if a.cfg != nil && !a.cfg.GetBool("features.show_search_sources", false) {
		return ""
	}
	lines := []string{"", "", "## Sources", "[grok2api-sources]: #"}
	for _, item := range a.webSearchResults {
		title := strings.ReplaceAll(asString(item["title"]), "\\", "\\\\")
		title = strings.ReplaceAll(title, "[", "\\[")
		title = strings.ReplaceAll(title, "]", "\\]")
		lines = append(lines, fmt.Sprintf("- [%s](%s)", title, asString(item["url"])))
	}
	return strings.Join(lines, "\n") + "\n"
}

func (a *StreamAdapter) AnnotationsList() []map[string]any {
	out := make([]map[string]any, 0, len(a.annotations))
	for _, item := range a.annotations {
		cloned := map[string]any{}
		for key, value := range item {
			cloned[key] = value
		}
		out = append(out, cloned)
	}
	return out
}

func (a *StreamAdapter) SearchSourcesList() []map[string]any {
	if len(a.webSearchResults) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(a.webSearchResults))
	for _, item := range a.webSearchResults {
		out = append(out, map[string]any{
			"url":   asString(item["url"]),
			"title": defaultIfEmpty(asString(item["title"]), asString(item["url"])),
			"type":  defaultIfEmpty(asString(item["type"]), "web"),
		})
	}
	return out
}

func (a *StreamAdapter) FinalText() string {
	return strings.TrimSpace(a.FinalMessage)
}

func (a *StreamAdapter) FinalError() string {
	if len(a.streamErrors) == 0 {
		return ""
	}
	return strings.TrimSpace(a.streamErrors[0])
}

func normalizeWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
