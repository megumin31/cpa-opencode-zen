package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatGeminiScenarioFixtures(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range chatGeminiScenarios {
		t.Run(name, func(t *testing.T) {
			if seen[name] {
				t.Fatal("duplicate scenario")
			}
			seen[name] = true
			c, ok := chatGeminiScenario(name)
			if !ok || c.stream == "" {
				t.Fatal("missing fixture")
			}
			for _, line := range strings.Split(c.stream, "\n") {
				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					if data != "[DONE]" && !json.Valid([]byte(data)) {
						t.Fatalf("invalid fixture JSON: %q", data)
					}
				}
			}
			if !c.failure {
				if err := validateSSE("chat", c.stream); err != nil {
					t.Fatalf("invalid successful upstream fixture: %v", err)
				}
			}
		})
	}
	if _, ok := chatGeminiScenario("chat-gemini-unknown"); ok {
		t.Fatal("unknown scenario accepted")
	}
}

func TestChatGeminiHTTPValidation(t *testing.T) {
	c, _ := chatGeminiScenario("chat-gemini-combined")
	usage := `,"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13,"cachedContentTokenCount":2,"thoughtsTokenCount":1}`
	valid := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]` + usage + "}\n\n"
	if err := validateChatGemini(c, 200, "text/event-stream", valid); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"production-shaped loss": strings.ReplaceAll(strings.ReplaceAll(valid, `,"finishReason":"STOP"`, ""), usage, ""),
		"missing usage":          strings.ReplaceAll(valid, usage, ""),
		"wrong usage":            strings.Replace(valid, `"promptTokenCount":10`, `"promptTokenCount":11`, 1),
		"missing usage field":    strings.Replace(valid, `,"thoughtsTokenCount":1`, "", 1),
		"null usage count":       strings.Replace(valid, `"totalTokenCount":13`, `"totalTokenCount":null`, 1),
		"extra usage field":      strings.Replace(valid, `"totalTokenCount":13`, `"totalTokenCount":13,"invented":1`, 1),
		"changed finish reason":  strings.Replace(valid, `"STOP"`, `"MAX_TOKENS"`, 1),
		"duplicate finish":       valid + valid,
		"data after finish":      valid + "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"later\"}]}}]}\n\n",
		"duplicate usage":        valid + "data: {" + strings.TrimPrefix(usage, ",") + "}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if validateChatGemini(c, 200, "text/event-stream", body) == nil {
				t.Fatal("accepted damaged Gemini HTTP stream")
			}
		})
	}
	for _, name := range []string{"chat-gemini-no-usage", "chat-gemini-partial-usage", "chat-gemini-zero-usage"} {
		t.Run(name, func(t *testing.T) {
			expect, _ := chatGeminiScenario(name)
			body := strings.Replace(valid, usage, "", 1)
			if expect.usage != nil {
				raw, _ := json.Marshal(expect.usage)
				body = strings.TrimSuffix(body, "}\n\n") + `,"usageMetadata":` + string(raw) + "}\n\n"
			}
			if err := validateChatGemini(expect, 200, "text/event-stream", body); err != nil {
				t.Fatal(err)
			}
			if validateChatGemini(expect, 200, "text/event-stream", valid) == nil {
				t.Fatal("accepted invented or changed usage values")
			}
		})
	}
}
