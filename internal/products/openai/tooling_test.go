package openai

import "testing"

func TestParseToolCallsFromXML(t *testing.T) {
	text := `<tool_calls><tool_call><tool_name>lookup_weather</tool_name><parameters>{"city":"Shanghai"}</parameters></tool_call></tool_calls>`
	calls := parseToolCalls(text, []string{"lookup_weather"})
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "lookup_weather" {
		t.Fatalf("unexpected tool name: %q", calls[0].Name)
	}
	if calls[0].Arguments != `{"city":"Shanghai"}` {
		t.Fatalf("unexpected tool arguments: %q", calls[0].Arguments)
	}
}

func TestParseResponsesInput(t *testing.T) {
	input := []any{
		map[string]any{
			"type":      "function_call",
			"name":      "lookup_weather",
			"call_id":   "call_1",
			"arguments": `{"city":"Shanghai"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "sunny",
		},
	}
	messages := parseResponsesInput(input)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0]["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %#v", messages[0]["role"])
	}
	if messages[1]["role"] != "tool" {
		t.Fatalf("expected tool role, got %#v", messages[1]["role"])
	}
}

func TestParseChatData(t *testing.T) {
	content, reasoning, stop := parseChatData(`{"result":{"response":{"token":"thinking","isThinking":true}}}`)
	if content != "" || reasoning != "thinking" || stop {
		t.Fatalf("unexpected thinking parse result: content=%q reasoning=%q stop=%v", content, reasoning, stop)
	}

	content, reasoning, stop = parseChatData(`{"result":{"response":{"token":"done","messageTag":"final"}}}`)
	if content != "done" || reasoning != "" || stop {
		t.Fatalf("unexpected final parse result: content=%q reasoning=%q stop=%v", content, reasoning, stop)
	}
}
