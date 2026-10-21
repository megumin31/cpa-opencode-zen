package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestGeminiHostChunksAreJSONNotSSE(t *testing.T) {
	for _, source := range []string{"gemini", "openai", "openai-response"} {
		t.Run(source, func(t *testing.T) {
			var wire, raw bytes.Buffer
			count := 0
			err := ForwardToHost(context.Background(), strings.NewReader(nativeFixture(source)), catalog.Route{Protocol: source, Alias: "opencode/p/m"}, translator.FormatGemini, []byte(clientRequest("gemini")), []byte(clientRequest(source)), func(chunk []byte) error {
				count++
				if !json.Valid(chunk) || bytes.Contains(chunk, []byte("\n")) {
					t.Fatalf("not a single JSON record: %q", chunk)
				}
				// Exact handler boundary for no alt / alt=sse / $alt=sse.
				wire.WriteString("data: ")
				wire.Write(chunk)
				wire.WriteString("\n\n")
				// Non-SSE alt is passed straight through by the same host handler.
				raw.Write(chunk)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			records := 0
			err = readSSE(context.Background(), &wire, func(e sseEvent) error {
				records++
				if !json.Valid(e.Data) {
					t.Fatalf("double-framed data: %q", e.Data)
				}
				return nil
			})
			if err != nil || count == 0 || records != count {
				t.Fatalf("records=%d count=%d err=%v", records, count, err)
			}
			for _, want := range []string{"hello", "weather", "Paris", "finishReason"} {
				if !strings.Contains(raw.String(), want) {
					t.Fatalf("lost %s: %s", want, raw.String())
				}
			}
		})
	}
}

func TestOtherHostFormatsKeepExactFrames(t *testing.T) {
	for _, source := range []string{"claude", "openai-response"} {
		route := catalog.Route{Protocol: source, Alias: "opencode/p/m"}
		var expected, actual bytes.Buffer
		request := []byte(clientRequest(source))
		if err := Forward(context.Background(), strings.NewReader(nativeFixture(source)), route, FormatName(source), request, request, func(b []byte) error { expected.Write(b); return nil }); err != nil {
			t.Fatal(err)
		}
		if err := ForwardToHost(context.Background(), strings.NewReader(nativeFixture(source)), route, FormatName(source), request, request, func(b []byte) error { actual.Write(b); return nil }); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(expected.Bytes(), actual.Bytes()) {
			t.Fatalf("changed %s frames", source)
		}
	}
}

func TestGeminiHostTextFinishAndMultilineJSON(t *testing.T) {
	for _, tc := range []struct{ source, input string }{
		{"openai", "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"gemini", "data: {\"candidates\":[{\"index\":0,\n" + "data: \"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}\n\n"},
	} {
		var chunks [][]byte
		err := ForwardToHost(context.Background(), strings.NewReader(tc.input), catalog.Route{Protocol: tc.source, Alias: "opencode/p/m"}, translator.FormatGemini, []byte(clientRequest("gemini")), []byte(clientRequest(tc.source)), func(b []byte) error { chunks = append(chunks, bytes.Clone(b)); return nil })
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range chunks {
			if !json.Valid(b) || bytes.Contains(b, []byte("\n")) {
				t.Fatalf("invalid host record %q", b)
			}
		}
		all := bytes.Join(chunks, nil)
		if !bytes.Contains(all, []byte("hello")) || !bytes.Contains(all, []byte(`"finishReason":"STOP"`)) {
			t.Fatalf("lost text/finish: %s", all)
		}
	}
}

// Test the executor boundary separately from the real HTTP handler integration
// in cmd/hostcheck/httpframing: internal wire frames are not host JSON chunks.
func TestChatHostChunksAreJSONWithoutDone(t *testing.T) {
	for _, source := range []string{"openai", "claude", "openai-response", "gemini"} {
		t.Run(source, func(t *testing.T) {
			var chunks [][]byte
			err := ForwardToHost(context.Background(), strings.NewReader(nativeFixture(source)), catalog.Route{Protocol: source, Alias: "opencode/p/m"}, translator.FormatOpenAI, []byte(clientRequest("openai")), []byte(clientRequest(source)), func(b []byte) error {
				if !json.Valid(b) || bytes.Contains(b, []byte("\n")) {
					t.Fatalf("host chunk is not compact JSON: %q", b)
				}
				chunks = append(chunks, bytes.Clone(b))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) == 0 {
				t.Fatal("empty stream")
			}
			all := string(bytes.Join(chunks, nil))
			for _, want := range []string{"hello", "weather", "Paris", "finish_reason"} {
				if !strings.Contains(all, want) {
					t.Fatalf("lost %s: %s", want, all)
				}
			}
		})
	}
}

func TestChatHostMultilineAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		wantError  bool
	}{
		{"multiline", "data: {\"id\":\"x\",\n" + "data: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", false},
		{"finish_then_eof", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n", false},
		{"truncated", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n", true},
		{"upstream_error", "data: {\"error\":{\"message\":\"fixture error\"}}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			err := ForwardToHost(context.Background(), strings.NewReader(tc.wire), catalog.Route{Protocol: "openai", Alias: "opencode/p/m"}, translator.FormatOpenAI, nil, nil, func(b []byte) error {
				count++
				if !json.Valid(b) {
					t.Fatalf("non-JSON: %q", b)
				}
				return nil
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if !tc.wantError && count != 1 {
				t.Fatalf("count=%d want one JSON chunk, no DONE", count)
			}
		})
	}
}

func TestGeminiToClaudeFlushesOnlyValidatedCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		wantError  bool
	}{
		{"complete", nativeFixture("gemini"), false},
		{"truncated", "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]}}]}\n\n", true},
		{"error_after_text", "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]}}]}\n\ndata: {\"error\":{\"message\":\"fixture failure\"}}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			err := ForwardToHost(context.Background(), strings.NewReader(tc.wire), catalog.Route{Protocol: "gemini", Alias: "opencode/p/m"}, translator.FormatClaude, []byte(clientRequest("claude")), []byte(clientRequest("gemini")), func(b []byte) error { wire.Write(b); return nil })
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			stops := 0
			if parseErr := readSSE(context.Background(), &wire, func(e sseEvent) error {
				var obj map[string]any
				if json.Unmarshal(e.Data, &obj) != nil {
					t.Fatalf("invalid JSON: %q", e.Data)
				}
				if obj["type"] == "message_stop" {
					stops++
				}
				return nil
			}); parseErr != nil {
				t.Fatal(parseErr)
			}
			want := 1
			if tc.wantError {
				want = 0
			}
			if stops != want {
				t.Fatalf("message_stop count=%d want=%d", stops, want)
			}
		})
	}
}

func TestResponsesGeminiCompletionRepair(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chunks    [][]byte
		eventType string
		want      string
	}{
		{"text", [][]byte{[]byte(`{"candidates":[{"index":0,"content":{"parts":[]}}],"usageMetadata":{"totalTokenCount":19}}`)}, "response.completed", "STOP"},
		{"incomplete", [][]byte{[]byte(`{"candidates":[{"finishReason":"MAX_TOKENS"}]}`)}, "response.incomplete", "MAX_TOKENS"},
		{"tool_completion_usage", [][]byte{[]byte(`{"usageMetadata":{"totalTokenCount":19}}`)}, "response.completed", "STOP"},
		{"tool_same_event", [][]byte{[]byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"weather"}}]}}]}`), []byte(`{"usageMetadata":{"totalTokenCount":19}}`)}, "response.completed", "STOP"},
		{"nonterminal", [][]byte{[]byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)}, "response.output_text.delta", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := completeResponsesGemini(tc.chunks, tc.eventType)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			reason := ""
			for _, b := range out {
				var o map[string]any
				if json.Unmarshal(b, &o) != nil {
					t.Fatal(string(b))
				}
				for _, v := range list(o["candidates"]) {
					if r, _ := object(v)["finishReason"].(string); r != "" {
						reason = r
						count++
					}
				}
			}
			if reason != tc.want || count > 1 {
				t.Fatalf("reason=%q count=%d", reason, count)
			}
			if bytes.Contains(bytes.Join(tc.chunks, nil), []byte("usageMetadata")) && !bytes.Contains(bytes.Join(out, nil), []byte("usageMetadata")) {
				t.Fatal("lost usage")
			}
		})
	}
}

func TestGeminiBlockedAndEmptyFeedback(t *testing.T) {
	for _, feedback := range []string{`{"blockReason":"SAFETY"}`, `{}`, `{"blockReason":"BLOCK_REASON_UNSPECIFIED"}`} {
		for _, out := range []translator.Format{translator.FormatOpenAI, translator.FormatClaude, translator.FormatOpenAIResponse, translator.FormatGemini} {
			t.Run(feedback+"/"+out.String(), func(t *testing.T) {
				emitted := 0
				err := ForwardToHost(context.Background(), strings.NewReader("data: {\"promptFeedback\":"+feedback+"}\n\n"), catalog.Route{Protocol: "gemini", Alias: "m"}, out, nil, nil, func(b []byte) error { emitted++; return nil })
				nativeBlocked := out == translator.FormatGemini && strings.Contains(feedback, "SAFETY")
				if (err == nil) != nativeBlocked {
					t.Fatalf("error=%v nativeBlocked=%v", err, nativeBlocked)
				}
				if strings.Contains(feedback, "SAFETY") && !nativeBlocked && emitted != 0 {
					t.Fatal("blocked response reached converter")
				}
			})
		}
	}
}

func TestGeminiToolDrainRejectsUnexpectedShapes(t *testing.T) {
	goodTool := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","id":"call_1","args":{}}}]}}]}`)
	goodSynthetic := []byte(`{"candidates":[{"content":{"parts":[{"thought":true,"text":""}]}}]}`)
	for _, tc := range []struct {
		name   string
		chunks [][]byte
		id     string
		good   bool
	}{
		{"valid", [][]byte{goodTool, goodSynthetic}, "call_1", true},
		{"missing tool", [][]byte{goodSynthetic}, "call_1", false},
		{"wrong identity", [][]byte{goodTool, goodSynthetic}, "other", false},
		{"real reasoning", [][]byte{goodTool, bytes.ReplaceAll(goodSynthetic, []byte(`"text":""`), []byte(`"text":"important"`))}, "call_1", false},
		{"extra record", [][]byte{goodTool, goodSynthetic, goodSynthetic}, "call_1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := drainedResponsesGeminiTool(tc.chunks, map[string]any{"call_id": tc.id})
			if (err == nil) != tc.good {
				t.Fatalf("err=%v", err)
			}
			if tc.good && (len(out) != 1 || !bytes.Equal(out[0], goodTool)) {
				t.Fatal("lost tool")
			}
		})
	}
}

func TestResponsesToolThenFailureHasNoGeminiFinish(t *testing.T) {
	created := `data: {"type":"response.created","response":{"id":"r","model":"m"}}` + "\n\n"
	tool := `data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"c1","name":"weather","arguments":"{}"}}` + "\n\n"
	text := `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"
	for _, end := range []string{"", `data: {"type":"response.failed","response":{"error":{"message":"failed"}}}` + "\n\n"} {
		var out bytes.Buffer
		err := ForwardToHost(context.Background(), strings.NewReader(created+tool+text+end), catalog.Route{Protocol: "openai-response", Alias: "m"}, translator.FormatGemini, []byte(clientRequest("gemini")), []byte(clientRequest("openai-response")), func(b []byte) error { out.Write(b); return nil })
		if err == nil {
			t.Fatal("missing upstream error")
		}
		if strings.Contains(out.String(), "finishReason") {
			t.Fatalf("premature finish: %s", out.String())
		}
		if !strings.Contains(out.String(), "weather") || !strings.Contains(out.String(), "hello") {
			t.Fatal("lost delivered content")
		}
	}
}
