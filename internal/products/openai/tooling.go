package openai

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

type ParsedToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

func buildToolSystemPrompt(tools []map[string]any, toolChoice any) string {
	toolDefs := make([]string, 0, len(tools))
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		description, _ := function["description"].(string)
		line := fmt.Sprintf("Tool: %s", name)
		if description != "" {
			line += "\nDescription: " + description
		}
		if params := function["parameters"]; params != nil {
			if payload, err := json.Marshal(params); err == nil {
				line += "\nParameters: " + string(payload)
			}
		}
		toolDefs = append(toolDefs, line)
	}
	choice := "WHEN TO CALL: Call a tool when it is clearly needed. Otherwise respond in plain text."
	switch typed := toolChoice.(type) {
	case string:
		switch typed {
		case "none":
			choice = "WHEN TO CALL: Do NOT call any tools. Respond in plain text only."
		case "required":
			choice = "WHEN TO CALL: You MUST output a <tool_calls> XML block. Do NOT write any plain-text reply under any circumstances."
		}
	case map[string]any:
		if typed["type"] == "function" {
			function, _ := typed["function"].(map[string]any)
			if name, _ := function["name"].(string); name != "" {
				choice = fmt.Sprintf("WHEN TO CALL: You MUST output a <tool_calls> XML block calling the tool named %q.", name)
			}
		}
	}
	return fmt.Sprintf(`You have access to the following tools.

AVAILABLE TOOLS:
%s

TOOL CALL FORMAT — follow these rules exactly:
- When calling a tool, output ONLY the XML block below. No text before or after it.
- <parameters> must be a single-line valid JSON object (no line breaks inside).
- Place multiple tool calls inside ONE <tool_calls> element.
- Do NOT use markdown code fences around the XML.

<tool_calls>
  <tool_call>
    <tool_name>TOOL_NAME</tool_name>
    <parameters>{"key":"value"}</parameters>
  </tool_call>
</tool_calls>

%s`, strings.Join(toolDefs, "\n\n"), choice)
}

func extractToolNames(tools []map[string]any) []string {
	out := []string{}
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) != "" {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

func injectToolPrompt(message, systemPrompt string) string {
	return "[system]: " + systemPrompt + "\n\n" + message
}

func toolCallsToXML(toolCalls []map[string]any) string {
	lines := []string{"<tool_calls>"}
	for _, tool := range toolCalls {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		arguments, _ := function["arguments"].(string)
		if parsed := normalizeJSON(arguments); parsed != "" {
			arguments = parsed
		}
		lines = append(lines,
			"  <tool_call>",
			"    <tool_name>"+name+"</tool_name>",
			"    <parameters>"+arguments+"</parameters>",
			"  </tool_call>",
		)
	}
	lines = append(lines, "</tool_calls>")
	return strings.Join(lines, "\n")
}

func normalizeJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

var (
	xmlRootRe   = regexp.MustCompile(`(?is)<tool_calls\s*>(.*?)</tool_calls\s*>`)
	xmlCallRe   = regexp.MustCompile(`(?is)<tool_call\s*>(.*?)</tool_call\s*>`)
	xmlNameRe   = regexp.MustCompile(`(?is)<tool_name\s*>(.*?)</tool_name\s*>`)
	xmlParamRe  = regexp.MustCompile(`(?is)<parameters\s*>(.*?)</parameters\s*>`)
	jsonObjRe   = regexp.MustCompile(`(?s)\{.*\}`)
	jsonArrayRe = regexp.MustCompile(`(?s)\[.*\]`)
)

func parseToolCalls(text string, available []string) []ParsedToolCall {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	root := xmlRootRe.FindStringSubmatch(text)
	if len(root) > 1 {
		calls := []ParsedToolCall{}
		for _, match := range xmlCallRe.FindAllStringSubmatch(root[1], -1) {
			nameMatch := xmlNameRe.FindStringSubmatch(match[1])
			if len(nameMatch) < 2 {
				continue
			}
			name := strings.TrimSpace(nameMatch[1])
			if len(available) > 0 && !containsString(available, name) {
				continue
			}
			parameters := "{}"
			if params := xmlParamRe.FindStringSubmatch(match[1]); len(params) > 1 {
				if normalized := normalizeJSON(strings.TrimSpace(params[1])); normalized != "" {
					parameters = normalized
				}
			}
			calls = append(calls, ParsedToolCall{
				CallID:    fmt.Sprintf("call_%d%s", time.Now().UnixMilli(), randomSuffix()),
				Name:      name,
				Arguments: parameters,
			})
		}
		return calls
	}
	if objMatch := jsonObjRe.FindString(text); objMatch != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(objMatch), &obj); err == nil {
			if toolCalls, ok := obj["tool_calls"].([]any); ok {
				return parseJSONToolCalls(toolCalls, available)
			}
		}
	}
	if arrMatch := jsonArrayRe.FindString(text); arrMatch != "" {
		var arr []any
		if err := json.Unmarshal([]byte(arrMatch), &arr); err == nil {
			return parseJSONToolCalls(arr, available)
		}
	}
	return nil
}

func parseJSONToolCalls(items []any, available []string) []ParsedToolCall {
	out := []ParsedToolCall{}
	for _, item := range items {
		mapped, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := mapped["name"].(string)
		if name == "" {
			name, _ = mapped["tool_name"].(string)
		}
		if name == "" || (len(available) > 0 && !containsString(available, name)) {
			continue
		}
		args := "{}"
		if value := mapped["input"]; value != nil {
			if data, err := json.Marshal(value); err == nil {
				args = string(data)
			}
		} else if value := mapped["arguments"]; value != nil {
			if typed, ok := value.(string); ok && normalizeJSON(typed) != "" {
				args = normalizeJSON(typed)
			}
		}
		out = append(out, ParsedToolCall{
			CallID:    fmt.Sprintf("call_%d%s", time.Now().UnixMilli(), randomSuffix()),
			Name:      name,
			Arguments: args,
		})
	}
	return out
}

func randomSuffix() string {
	return fmt.Sprintf("%x", os.Getpid())[:3]
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
