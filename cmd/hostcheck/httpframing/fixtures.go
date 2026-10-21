package main

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Protocol metadata is a closed allowlist, never a URL supplied by a capture.
type protocolSpec struct {
	path, basePath, packageName, authHeader, authValue, stream string
	format                                                     translator.Format
}

func protocolSpecFor(protocol string) (protocolSpec, error) {
	s := protocolSpec{authHeader: "authorization", authValue: "Bearer " + fakeKey}
	switch protocol {
	case "chat":
		s.path, s.basePath, s.packageName, s.format = "/inference/openai/v1/chat/completions", "/inference/openai/v1", "@ai-sdk/openai-compatible", translator.FormatOpenAI
		s.stream = `data: {"id":"fake-wire-response","object":"chat.completion.chunk","model":"space-bunny-free","choices":[{"index":0,"delta":{"role":"assistant","content":"OK"},"finish_reason":null}]}

data: {"id":"fake-wire-response","object":"chat.completion.chunk","model":"space-bunny-free","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}

data: [DONE]

`
	case "responses":
		s.path, s.basePath, s.packageName, s.format = "/inference/openai/v1/responses", "/inference/openai/v1", "@ai-sdk/openai", translator.FormatOpenAIResponse
		s.stream = `event: response.created
data: {"type":"response.created","response":{"id":"fake-wire-response","object":"response","created_at":1,"model":"space-bunny-free","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fake-message","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"fake-message","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"fake-message","output_index":0,"content_index":0,"delta":"OK"}

event: response.output_text.done
data: {"type":"response.output_text.done","item_id":"fake-message","output_index":0,"content_index":0,"text":"OK"}

event: response.content_part.done
data: {"type":"response.content_part.done","item_id":"fake-message","output_index":0,"content_index":0,"part":{"type":"output_text","text":"OK","annotations":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fake-message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"fake-wire-response","object":"response","status":"completed","model":"space-bunny-free","output":[{"id":"fake-message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}

`
	case "anthropic":
		s.path, s.basePath, s.packageName, s.format = "/inference/anthropic/v1/messages", "/inference/anthropic/v1", "@ai-sdk/anthropic", translator.FormatClaude
		s.authHeader, s.authValue = "x-api-key", fakeKey
		s.stream = `event: message_start
data: {"type":"message_start","message":{"id":"fake-message","type":"message","role":"assistant","model":"space-bunny-free","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	case "gemini":
		s.path, s.basePath, s.packageName, s.format = "/inference/google/v1beta/models/space-bunny-free:streamGenerateContent?alt=sse", "/inference/google/v1beta", "@ai-sdk/google", translator.FormatGemini
		s.authHeader, s.authValue = "x-goog-api-key", fakeKey
		s.stream = `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11},"modelVersion":"space-bunny-free","responseId":"fake-wire-response"}

`
	default:
		return protocolSpec{}, fmt.Errorf("unsupported protocol %q", protocol)
	}
	return s, nil
}
