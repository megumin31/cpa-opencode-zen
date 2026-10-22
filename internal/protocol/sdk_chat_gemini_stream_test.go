package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/megumin31/cpa-opencode-zen/internal/fault"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func chatGeminiTestFrame(delta, finish, usage string) string {
	var d any
	_ = json.Unmarshal([]byte(delta), &d)
	choice := map[string]any{"index": 0, "delta": d}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	obj := map[string]any{"id": "test-chat", "model": "m", "choices": []any{choice}}
	if usage != "" {
		var u any
		_ = json.Unmarshal([]byte(usage), &u)
		obj["usage"] = u
	}
	raw, _ := json.Marshal(obj)
	return "data: " + string(raw) + "\n\n"
}

func TestChatGeminiCoalescedStreamSemantics(t *testing.T) {
	const usage = `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"completion_tokens_details":{"reasoning_tokens":1},"prompt_tokens_details":{"cached_tokens":3}}`
	const expectedUsage = `{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12,"thoughtsTokenCount":1,"cachedContentTokenCount":3}`
	const done = "data: [DONE]\n\n"
	text := chatGeminiTestFrame(`{"content":"OK"}`, "", "")
	for _, tc := range []struct {
		name, wire, finish, usage, thought string
		tool                               bool
	}{
		{name: "combined", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", usage) + done, finish: "STOP", usage: expectedUsage},
		{name: "finish_usage", wire: text + chatGeminiTestFrame(`{}`, "stop", usage) + done, finish: "STOP", usage: expectedUsage},
		{name: "content_usage", wire: chatGeminiTestFrame(`{"content":"OK"}`, "", usage) + chatGeminiTestFrame(`{}`, "stop", "") + done, finish: "STOP", usage: expectedUsage},
		{name: "usage_only", wire: text + chatGeminiTestFrame(`{}`, "stop", "") + "data: {\"choices\":[],\"usage\":" + usage + "}\n\n" + done, finish: "STOP", usage: expectedUsage},
		{name: "clean_eof", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", usage), finish: "STOP", usage: expectedUsage},
		{name: "length", wire: chatGeminiTestFrame(`{"content":"OK"}`, "length", usage) + done, finish: "MAX_TOKENS", usage: expectedUsage},
		{name: "filtered", wire: chatGeminiTestFrame(`{"content":"OK"}`, "content_filter", usage) + done, finish: "SAFETY", usage: expectedUsage},
		{name: "reasoning", wire: chatGeminiTestFrame(`{"reasoning_content":"think","content":"OK"}`, "stop", usage) + done, finish: "STOP", usage: expectedUsage, thought: "think"},
		{name: "mixed_tool", wire: chatGeminiTestFrame(`{"reasoning_content":"think","content":"OK","tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]}`, "tool_calls", usage) + done, finish: "STOP", usage: expectedUsage, thought: "think", tool: true},
		{name: "tool_last_piece", wire: text + chatGeminiTestFrame(`{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]}`, "", "") + chatGeminiTestFrame(`{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}`, "tool_calls", usage) + done, finish: "STOP", usage: expectedUsage, tool: true},
		{name: "no_usage", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", "") + done, finish: "STOP"},
		{name: "null_usage", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", "null") + done, finish: "STOP"},
		{name: "partial_usage", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", `{"prompt_tokens":10}`) + done, finish: "STOP", usage: `{"promptTokenCount":10}`},
		{name: "no_reported_total", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", `{"prompt_tokens":10,"completion_tokens":2}`) + done, finish: "STOP", usage: `{"promptTokenCount":10,"candidatesTokenCount":2}`},
		{name: "zero_usage_aliases", wire: chatGeminiTestFrame(`{"content":"OK"}`, "stop", `{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`) + done, finish: "STOP", usage: `{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0,"cachedContentTokenCount":0,"thoughtsTokenCount":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chunks [][]byte
			err := ForwardToHost(context.Background(), strings.NewReader(tc.wire), catalog.Route{Protocol: "openai", Alias: "m"}, translator.FormatGemini, []byte(clientRequest("gemini")), []byte(clientRequest("openai")), func(b []byte) error {
				chunks = append(chunks, bytes.Clone(b))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var text, thought, finish string
			var usage map[string]any
			finishes, usages, tools := 0, 0, 0
			for _, raw := range chunks {
				var obj map[string]any
				if err := json.Unmarshal(raw, &obj); err != nil {
					t.Fatal(err)
				}
				if value, exists := obj["usageMetadata"]; exists {
					usages++
					usage = object(value)
				}
				for _, value := range list(obj["candidates"]) {
					candidate := object(value)
					if reason, _ := candidate["finishReason"].(string); reason != "" {
						finishes++
						finish = reason
					}
					for _, value := range list(object(candidate["content"])["parts"]) {
						part := object(value)
						partText, _ := part["text"].(string)
						if part["thought"] == true {
							thought += partText
						} else {
							text += partText
						}
						if call := object(part["functionCall"]); call != nil {
							tools++
							if call["id"] != "call_weather" || call["name"] != "weather" || object(call["args"])["city"] != "Paris" {
								t.Fatalf("lost tool payload: %s", raw)
							}
						}
					}
				}
			}
			var wantUsage map[string]any
			wantUsages, wantTools := 0, 0
			if tc.usage != "" {
				_ = json.Unmarshal([]byte(tc.usage), &wantUsage)
				wantUsages = 1
			}
			if tc.tool {
				wantTools = 1
			}
			if text != "OK" || thought != tc.thought || finishes != 1 || finish != tc.finish || tools != wantTools || usages != wantUsages || !reflect.DeepEqual(usage, wantUsage) {
				t.Fatalf("text=%q thought=%q finish=%q/%d tools=%d usage=%v/%d; chunks=%s", text, thought, finish, finishes, tools, usage, usages, bytes.Join(chunks, []byte("\n")))
			}
		})
	}
}

func TestChatGeminiFailuresDoNotInventCompletion(t *testing.T) {
	for _, tc := range []struct{ name, wire string }{
		{"truncated", chatGeminiTestFrame(`{"content":"OK"}`, "", "")},
		{"error", chatGeminiTestFrame(`{"content":"OK"}`, "", "") + "data: {\"error\":{\"message\":\"synthetic failure\"}}\n\n"},
		{"error_finish", chatGeminiTestFrame(`{"content":"OK"}`, "error", "")},
		{"network_error_finish", chatGeminiTestFrame(`{"content":"OK"}`, "network_error", "")},
		{"refusal", chatGeminiTestFrame(`{"refusal":"Cannot help"}`, "content_filter", "")},
		{"legacy_function_call", chatGeminiTestFrame(`{"function_call":{"name":"weather","arguments":"{}"}}`, "function_call", "")},
		{"invalid_usage", chatGeminiTestFrame(`{"content":"OK"}`, "stop", `{"prompt_tokens":"unknown"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chunks bytes.Buffer
			err := ForwardToHost(context.Background(), strings.NewReader(tc.wire), catalog.Route{Protocol: "openai", Alias: "m"}, translator.FormatGemini, nil, nil, func(b []byte) error { chunks.Write(b); return nil })
			if err == nil || strings.Contains(chunks.String(), "finishReason") {
				t.Fatalf("error=%v output=%s", err, chunks.String())
			}
		})
	}
}

func TestChatGeminiCancelAfterCoalescedContent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var chunks bytes.Buffer
	wire := chatGeminiTestFrame(`{"content":"OK"}`, "stop", `{"total_tokens":12}`) + "data: [DONE]\n\n"
	err := ForwardToHost(ctx, strings.NewReader(wire), catalog.Route{Protocol: "openai", Alias: "m"}, translator.FormatGemini, nil, nil, func(b []byte) error {
		chunks.Write(b)
		cancel()
		return nil
	})
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Status != 499 || strings.Contains(chunks.String(), "finishReason") || strings.Contains(chunks.String(), "usageMetadata") {
		t.Fatalf("error=%v output=%s", err, chunks.String())
	}
}
