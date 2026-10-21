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
	for _, source := range []string{"openai", "claude", "openai-response"} {
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
