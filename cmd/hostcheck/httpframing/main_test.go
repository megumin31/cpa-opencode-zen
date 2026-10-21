package main

import (
	"os"
	"strings"
	"testing"
)

func TestStrictNativeSSEFixtures(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			spec, err := protocolSpecFor(protocol)
			if err != nil {
				t.Fatal(err)
			}
			if err = validateSSE(protocol, spec.stream); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStrictSSERejectsFramingRegressions(t *testing.T) {
	chat, _ := protocolSpecFor("chat")
	cases := map[string]string{
		"double data prefix": strings.Replace(chat.stream, "data: {", "data: data: {", 1),
		"duplicate done":     chat.stream + "data: [DONE]\n\n",
		"missing done":       strings.ReplaceAll(chat.stream, "data: [DONE]\n\n", ""),
		"missing finish":     strings.ReplaceAll(chat.stream, `"finish_reason":"stop"`, `"finish_reason":null`),
		"truncated frame":    strings.TrimSuffix(chat.stream, "\n"),
		"error":              chat.stream + "data: {\"error\":{\"message\":\"truncated\"}}\n\n",
		"non json":           chat.stream + "data: nonsense\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateSSE("chat", body); err == nil {
				t.Fatal("accepted malformed stream")
			}
		})
	}
}

func TestStrictNamedEvents(t *testing.T) {
	for _, p := range []string{"responses", "anthropic"} {
		t.Run(p, func(t *testing.T) {
			spec, _ := protocolSpecFor(p)
			wrong := strings.Replace(spec.stream, "event: ", "event: wrong_", 1)
			if validateSSE(p, wrong) == nil {
				t.Fatal("accepted mismatched event type")
			}
			if validateSSE(p, spec.stream+"data: [DONE]\n\n") == nil {
				t.Fatal("accepted foreign done marker")
			}
		})
	}
}

// The compiled-library run is opt-in so ordinary offline unit tests do not
// silently reuse a stale binary. CI invokes the standalone harness after build.
func TestNativeHTTPMatrix(t *testing.T) {
	plugin := os.Getenv("HTTPFRAMING_PLUGIN")
	if plugin == "" {
		t.Skip("set HTTPFRAMING_PLUGIN to a freshly built native library")
	}
	if err := run(plugin, true); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalOrdering(t *testing.T) {
	chat, _ := protocolSpecFor("chat")
	response, _ := protocolSpecFor("responses")
	claude, _ := protocolSpecFor("anthropic")
	cases := []struct{ name, protocol, body string }{
		{"chat event after done", "chat", chat.stream + "data: {}\n\n"},
		{"chat premature done", "chat", "data: [DONE]\n\n" + chat.stream},
		{"response event after completed", "responses", response.stream + "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"later\"}\n\n"},
		{"claude event after stop", "anthropic", claude.stream + "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"later\"}}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if validateSSE(tc.protocol, tc.body) == nil {
				t.Fatal("accepted event out of terminal order")
			}
		})
	}
	gemini, _ := protocolSpecFor("gemini")
	if err := validateSSE("gemini", gemini.stream+"data: {\"usageMetadata\":{\"totalTokenCount\":11}}\n\n"); err != nil {
		t.Fatalf("valid Gemini trailing usage: %v", err)
	}
}

func TestFailureValidationRejectsFalseSuccess(t *testing.T) {
	if err := validateFailure("chat", 502, `{"error":{"message":"failed"}}`); err != nil {
		t.Fatal(err)
	}
	failure := "event: error\ndata: {\"error\":{\"message\":\"failed\"}}\n\n"
	if err := validateFailure("chat", 200, failure); err != nil {
		t.Fatal(err)
	}
	for _, p := range protocols {
		t.Run(p, func(t *testing.T) {
			s, _ := protocolSpecFor(p)
			if validateFailure(p, 200, failure+s.stream) == nil {
				t.Fatal("accepted synthesized success after failure")
			}
		})
	}
	for _, p := range protocols {
		t.Run(p+"_truncated_fixture", func(t *testing.T) {
			s, _ := protocolSpecFor(p)
			if validateSSE(p, scenarioStream(p, "truncated", s.stream)) == nil {
				t.Fatal("truncated fixture is accidentally complete")
			}
		})
	}
}
