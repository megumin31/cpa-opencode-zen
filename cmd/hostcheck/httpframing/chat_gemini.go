package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// Keep these fixtures at the real native-host HTTP boundary: the pinned SDK
// can return early after text, reasoning or tools and silently lose fields
// carried on that same OpenAI chunk.
type chatGeminiCase struct {
	stream, finish, reasoning string
	usage                     map[string]int64
	tool, failure, cancel     bool
}

var chatGeminiScenarios = []string{
	"chat-gemini-combined", "chat-gemini-reasoning", "chat-gemini-split-usage",
	"chat-gemini-no-usage", "chat-gemini-partial-usage", "chat-gemini-zero-usage",
	"chat-gemini-length", "chat-gemini-filtered", "chat-gemini-tools",
	"chat-gemini-error", "chat-gemini-truncated", "chat-gemini-cancel",
	"chat-gemini-refusal", "chat-gemini-legacy-function-call",
}

func chatGeminiScenario(name string) (chatGeminiCase, bool) {
	usage := `{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}`
	c := chatGeminiCase{finish: "STOP", usage: map[string]int64{
		"promptTokenCount": 10, "candidatesTokenCount": 3, "totalTokenCount": 13,
		"cachedContentTokenCount": 2, "thoughtsTokenCount": 1,
	}}
	content, finish := `{"content":"OK"}`, `"stop"`
	done := "data: [DONE]\n\n"
	switch name {
	case "chat-gemini-combined":
	case "chat-gemini-reasoning":
		content = `{"reasoning_content":"think","content":"OK"}`
		c.reasoning = "think"
	case "chat-gemini-split-usage":
		c.stream = chatGeminiChunk(content, finish, "") + "data: {\"choices\":[],\"usage\":" + usage + "}\n\n" + done
		return c, true
	case "chat-gemini-no-usage":
		usage, c.usage = "", nil
	case "chat-gemini-partial-usage":
		usage = `{"prompt_tokens":7,"prompt_tokens_details":{"cached_tokens":0}}`
		c.usage = map[string]int64{"promptTokenCount": 7, "cachedContentTokenCount": 0}
	case "chat-gemini-zero-usage":
		usage = `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}`
		c.usage = map[string]int64{"promptTokenCount": 0, "candidatesTokenCount": 0, "totalTokenCount": 0, "cachedContentTokenCount": 0, "thoughtsTokenCount": 0}
	case "chat-gemini-length":
		finish, c.finish = `"length"`, "MAX_TOKENS"
	case "chat-gemini-filtered":
		finish, c.finish = `"content_filter"`, "SAFETY"
	case "chat-gemini-tools":
		c.tool = true
		c.stream = chatGeminiChunk(content, "null", "") +
			chatGeminiChunk(`{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"value\":"}}]}`, "null", "") +
			chatGeminiChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}`, `"tool_calls"`, usage) + done
		return c, true
	case "chat-gemini-refusal", "chat-gemini-legacy-function-call":
		c.failure, c.usage, c.finish = true, nil, ""
		delta, reason := `{"refusal":"Cannot comply"}`, `"content_filter"`
		if name == "chat-gemini-legacy-function-call" {
			delta, reason = `{"function_call":{"name":"weather","arguments":"{}"}}`, `"function_call"`
		}
		c.stream = chatGeminiChunk(content, "null", "") + chatGeminiChunk(delta, reason, usage) + done
		return c, true
	case "chat-gemini-error", "chat-gemini-truncated", "chat-gemini-cancel":
		c.failure, c.cancel, c.usage, c.finish = true, name == "chat-gemini-cancel", nil, ""
		c.stream = chatGeminiChunk(content, "null", "")
		if name == "chat-gemini-error" {
			c.stream += "event: error\ndata: {\"error\":{\"type\":\"server_error\",\"message\":\"synthetic failure after text\"}}\n\n"
		}
		return c, true
	default:
		return chatGeminiCase{}, false
	}
	c.stream = chatGeminiChunk(content, finish, usage) + done
	return c, true
}

func chatGeminiChunk(delta, finish, usage string) string {
	extra := ""
	if usage != "" {
		extra = `,"usage":` + usage
	}
	return fmt.Sprintf("data: {\"id\":\"fake-wire-response\",\"object\":\"chat.completion.chunk\",\"model\":\"space-bunny-free\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%s}]%s}\n\n", delta, finish, extra)
}

func validateChatGemini(c chatGeminiCase, status int, contentType, body string) error {
	if status != 200 || !strings.HasPrefix(contentType, "text/event-stream") {
		return fmt.Errorf("HTTP %d content-type=%q", status, contentType)
	}
	if err := validateSSE("gemini", body); err != nil {
		return err
	}
	finished, usageCount, tools := false, 0, 0
	var text, reasoning strings.Builder
	for _, frame := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			j := gjson.Parse(strings.TrimPrefix(line, "data: "))
			for _, candidate := range j.Get("candidates").Array() {
				parts := candidate.Get("content.parts").Array()
				if finished && len(parts) != 0 {
					return errors.New("content emitted after Gemini finishReason")
				}
				for _, part := range parts {
					if value := part.Get("text"); value.Exists() {
						if part.Get("thought").Bool() {
							reasoning.WriteString(value.String())
						} else {
							text.WriteString(value.String())
						}
					}
					if tool := part.Get("functionCall"); tool.Exists() {
						tools++
						if tool.Get("name").String() != "weather" || tool.Get("id").String() != "call_weather" || tool.Get("args.value").Int() != 1 {
							return fmt.Errorf("tool identity/arguments changed: %s", tool.Raw)
						}
					}
				}
				if reason := candidate.Get("finishReason").String(); reason != "" {
					if finished || reason != c.finish {
						return fmt.Errorf("finishReason=%q want=%q duplicate=%v", reason, c.finish, finished)
					}
					finished = true
				}
			}
			if usage := j.Get("usageMetadata"); usage.Exists() {
				usageCount++
				if c.usage == nil {
					return errors.New("invented usageMetadata when upstream omitted usage")
				}
				var fields map[string]json.RawMessage
				if json.Unmarshal([]byte(usage.Raw), &fields) != nil || len(fields) != len(c.usage) {
					return fmt.Errorf("usage field set changed: %s", usage.Raw)
				}
				for name, want := range c.usage {
					value := usage.Get(name)
					if value.Type != gjson.Number || value.Num != float64(want) {
						return fmt.Errorf("usage %s=%s want=%d", name, value.Raw, want)
					}
				}
			}
		}
	}
	wantUsage, wantTools := 0, 0
	if c.usage != nil {
		wantUsage = 1
	}
	if c.tool {
		wantTools = 1
	}
	if !finished || text.String() != "OK" || reasoning.String() != c.reasoning || tools != wantTools || usageCount != wantUsage {
		return fmt.Errorf("finish=%v text=%q reasoning=%q tools=%d/%d usage=%d/%d", finished, text.String(), reasoning.String(), tools, wantTools, usageCount, wantUsage)
	}
	return nil
}
