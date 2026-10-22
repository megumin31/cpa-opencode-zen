package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/megumin31/cpa-opencode-zen/internal/fault"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The pinned Chat-to-Gemini SDK returns after text/reasoning, tool deltas, or
// finish_reason, silently skipping the remaining fields of a coalesced record.
// Feed those semantic groups separately without changing the upstream wire or
// inventing a finish. Usage is emitted once by prepareChatGeminiStream instead
// of passing through the SDK's lossy/defaulting usage conversion.
func translationRecords(up, out translator.Format, data []byte) [][]byte {
	if up != translator.FormatOpenAI || out != translator.FormatGemini {
		return [][]byte{data}
	}
	var obj map[string]any
	if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:"))), &obj) != nil || obj == nil {
		return [][]byte{data}
	}
	var tools, finishes []any
	for _, value := range list(obj["choices"]) {
		choice := object(value)
		delta := object(choice["delta"])
		if calls, exists := delta["tool_calls"]; exists {
			tools = append(tools, map[string]any{"index": choice["index"], "delta": map[string]any{"tool_calls": calls}})
			delete(delta, "tool_calls")
		}
		if reason, _ := choice["finish_reason"].(string); reason != "" {
			finishes = append(finishes, map[string]any{"index": choice["index"], "delta": map[string]any{}, "finish_reason": reason})
			delete(choice, "finish_reason")
		}
	}
	delete(obj, "usage")
	raw, _ := json.Marshal(obj)
	result := [][]byte{append([]byte("data: "), raw...)}
	for _, choices := range [][]any{tools, finishes} {
		if len(choices) == 0 {
			continue
		}
		record := catalog.Merge(nil, obj)
		record["choices"] = choices
		raw, _ := json.Marshal(record)
		result = append(result, append([]byte("data: "), raw...))
	}
	return result
}

// Only convert counters actually reported by this source event. In particular,
// absent/null usage is not zero usage, and partial usage is not a reported total.
// Refusals and legacy function_call payloads cannot be represented safely by
// the pinned streaming converter; reject them before emitting an empty finish.
func prepareChatGeminiStream(obj map[string]any) ([]byte, error) {
	for _, value := range list(obj["choices"]) {
		delta := object(object(value)["delta"])
		if refusal := delta["refusal"]; refusal != nil && refusal != "" {
			return nil, fault.New(502, "upstream Chat refusal cannot be safely converted to Gemini")
		}
		if delta["function_call"] != nil {
			return nil, fault.New(502, "upstream legacy function_call cannot be safely converted to Gemini")
		}
	}
	if obj["usage"] == nil {
		return nil, nil
	}
	usage := object(obj["usage"])
	if usage == nil {
		return nil, fault.New(502, "invalid upstream Chat usage")
	}
	metadata := map[string]any{}
	for _, field := range []struct {
		target string
		source []string
	}{
		{"promptTokenCount", []string{"prompt_tokens", "input_tokens"}},
		{"candidatesTokenCount", []string{"completion_tokens", "output_tokens"}},
		{"totalTokenCount", []string{"total_tokens"}},
		{"thoughtsTokenCount", []string{"completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens"}},
		{"cachedContentTokenCount", []string{"prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens"}},
	} {
		for _, path := range field.source {
			var value any = usage
			for _, key := range strings.Split(path, ".") {
				value = object(value)[key]
			}
			if value == nil {
				continue
			}
			count, valid := value.(float64)
			if !valid || count < 0 || math.Trunc(count) != count {
				return nil, fault.New(502, "invalid upstream Chat token count")
			}
			metadata[field.target] = count
			break
		}
	}
	if len(metadata) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]any{"candidates": []any{}, "usageMetadata": metadata})
}
