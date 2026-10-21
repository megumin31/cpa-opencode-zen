package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/megumin31/cpa-opencode-zen/internal/fault"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// CPA registers provider-side Responses converters under codex. The wire
// protocol is Responses, but its Codex request defaults are not appropriate for
// a generic Console Responses route. Restore explicit sampling/output limits
// and remove unsolicited Codex defaults before native provider options apply.
func sdkUpstream(up, other translator.Format) translator.Format {
	if up == translator.FormatOpenAIResponse && other != up {
		return translator.FormatCodex
	}
	return up
}
func translateUpstreamRequest(ctx context.Context, source, up translator.Format, model string, raw []byte) ([]byte, error) {
	if source == up {
		return append([]byte(nil), raw...), nil
	}
	target := sdkUpstream(up, source)
	if !registry.HasRequestTransformer(source, target) {
		return nil, fault.New(400, "requested input protocol conversion is unavailable")
	}
	conversionRaw := raw
	var preservation *sdkResponsesChatState
	if source == translator.FormatOpenAIResponse && up == translator.FormatOpenAI {
		var err error
		conversionRaw, preservation, err = sdkProtectResponsesChat(raw)
		if err != nil {
			return nil, err
		}
	}
	translated := registry.TranslateRequestEnvelope(ctx, source, target, translator.RequestEnvelope{Format: source, Model: model, Stream: true, Body: conversionRaw})
	if translated.Err != nil {
		return nil, fault.New(400, "request cannot be translated without losing required tool data")
	}
	var src, out map[string]any
	if json.Unmarshal(raw, &src) != nil || json.Unmarshal(translated.Body, &out) != nil {
		return nil, fault.New(400, "invalid protocol conversion")
	}
	if err := preservation.restore(out); err != nil {
		return nil, err
	}
	if src["safetySettings"] != nil && up != translator.FormatGemini {
		return nil, fault.New(400, "target protocol cannot preserve explicit safetySettings")
	}
	if err := sdkPreserveGeneration(src, out, source, up); err != nil {
		return nil, err
	}
	if err := sdkValidateReasoningControls(src, out); err != nil {
		return nil, err
	}
	if err := sdkValidateContentPreserved(src, out); err != nil {
		return nil, err
	}
	if up == translator.FormatGemini {
		delete(out, "safetySettings")
		if value, ok := src["safetySettings"]; ok {
			out["safetySettings"] = catalog.Clone(value)
		}
	}
	if up == translator.FormatClaude {
		delete(out, "metadata")
		if value, ok := src["metadata"]; ok {
			metadata, valid := value.(map[string]any)
			if !valid {
				return nil, fault.New(400, "Anthropic metadata must be an object")
			}
			if err := nativeFields(metadata, "user_id", "Anthropic metadata"); err != nil {
				return nil, err
			}
			out["metadata"] = catalog.Clone(metadata)
		}
		if user, present := src["user"]; present && user != nil {
			if !nativeKind(user, "string") {
				return nil, fault.New(400, "explicit user must be a string")
			}
			metadata := object(out["metadata"])
			if metadata == nil {
				metadata = map[string]any{}
			}
			if _, exists := metadata["user_id"]; !exists {
				metadata["user_id"] = user
			}
			out["metadata"] = metadata
		}
	}
	if up != translator.FormatOpenAIResponse {
		return json.Marshal(out)
	}
	generation := src
	mapping := map[string]string{"temperature": "temperature", "top_p": "top_p", "max_tokens": "max_output_tokens", "max_completion_tokens": "max_output_tokens"}
	if source == translator.FormatGemini {
		generation = object(src["generationConfig"])
		mapping = map[string]string{"temperature": "temperature", "topP": "top_p", "maxOutputTokens": "max_output_tokens"}
	}
	for from, to := range mapping {
		if value, ok := generation[from]; ok {
			out[to] = catalog.Clone(value)
		}
	}
	if v, ok := src["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := src["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
	} else if source == translator.FormatClaude && object(src["tool_choice"])["disable_parallel_tool_use"] != nil {
		disable, ok := object(src["tool_choice"])["disable_parallel_tool_use"].(bool)
		if !ok {
			return nil, fault.New(400, "invalid disable_parallel_tool_use")
		}
		out["parallel_tool_calls"] = !disable
	} else {
		delete(out, "parallel_tool_calls")
	}
	if v, ok := src["service_tier"]; ok {
		out["service_tier"] = v
	}
	if src["stop"] != nil || src["stop_sequences"] != nil || generation["stopSequences"] != nil {
		return nil, fault.New(400, "Responses does not support requested stop sequences")
	}
	explicitReasoning := src["reasoning_effort"] != nil || src["thinking"] != nil || object(src["generationConfig"])["thinkingConfig"] != nil
	if !explicitReasoning {
		if reasoning := object(out["reasoning"]); reasoning != nil {
			delete(reasoning, "effort")
			if len(reasoning) == 0 {
				delete(out, "reasoning")
			}
		}
	}
	if generation["top_k"] != nil || generation["topK"] != nil {
		return nil, fault.New(400, "Responses does not support the requested top-k sampling option")
	}
	return json.Marshal(out)
}
func sdkNonStreamBody(up, out translator.Format, body []byte) []byte {
	if up != translator.FormatOpenAIResponse || out == up {
		return body
	}
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return nil
	}
	typ := "response.completed"
	if response["status"] == "incomplete" {
		typ = "response.incomplete"
	}
	raw, _ := json.Marshal(map[string]any{"type": typ, "response": response})
	return raw
}

// SDK converters use reasoning_content as their canonical Chat field. Preserve
// native frames unchanged; normalize only the converter's private copy.
func sdkResponseBody(r catalog.Route, body []byte) ([]byte, error) {
	if r.Protocol != "openai" {
		return body, nil
	}
	field, _ := r.Compatibility["reasoningField"].(string)
	if field == "" || field == "reasoning_content" {
		return body, nil
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body, nil
	}
	for _, choice := range list(obj["choices"]) {
		c := object(choice)
		for _, key := range []string{"delta", "message"} {
			m := object(c[key])
			if value, ok := m[field]; ok {
				if other, exists := m["reasoning_content"]; exists {
					a, _ := json.Marshal(other)
					b, _ := json.Marshal(value)
					if string(a) != string(b) {
						return nil, fault.New(502, "conflicting upstream reasoning fields")
					}
				}
				m["reasoning_content"] = value
				delete(m, field)
			}
		}
	}
	return json.Marshal(obj)
}

// The pinned Responses-to-Gemini SDK assigns STOP to individual tool records,
// but omits it for completed text. Only upstream terminal events may produce a
// finish; put exactly one reason on the last (usage) record of that event.
func completeResponsesGemini(chunks [][]byte, eventType string) ([][]byte, error) {
	terminal := eventType == "response.completed" || eventType == "response.incomplete"
	objects := make([]map[string]any, 0, len(chunks))
	reason := ""
	for _, chunk := range chunks {
		var obj map[string]any
		if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(chunk), []byte("data:"))), &obj) != nil || obj == nil {
			return nil, fault.New(502, "invalid converted Gemini stream payload")
		}
		for _, c := range list(obj["candidates"]) {
			candidate := object(c)
			if r, _ := candidate["finishReason"].(string); r != "" {
				reason = r
			}
			delete(candidate, "finishReason")
		}
		objects = append(objects, obj)
	}
	if terminal {
		if eventType == "response.completed" {
			reason = "STOP"
		} else if reason == "" {
			return nil, fault.New(502, "missing converted Gemini incomplete reason")
		}
		if len(objects) == 0 {
			objects = append(objects, map[string]any{})
		}
		last := objects[len(objects)-1]
		candidates := list(last["candidates"])
		if len(candidates) == 0 {
			candidates = []any{map[string]any{"index": 0}}
		}
		candidate := object(candidates[0])
		if candidate == nil {
			candidate = map[string]any{"index": 0}
			candidates[0] = candidate
		}
		candidate["finishReason"] = reason
		last["candidates"] = candidates
	}
	out := make([][]byte, 0, len(objects))
	for _, obj := range objects {
		raw, err := json.Marshal(obj)
		if err != nil {
			return nil, fault.New(502, "invalid converted Gemini completion")
		}
		out = append(out, raw)
	}
	return out, nil
}

// Drain only the exact two-record shape of the pinned SDK. A future SDK change
// must fail closed rather than lose a tool or leak our synthetic reasoning event.
func drainedResponsesGeminiTool(chunks [][]byte, item map[string]any) ([][]byte, error) {
	invalid := func() ([][]byte, error) { return nil, fault.New(502, "unexpected Gemini tool converter drain") }
	if len(chunks) != 2 {
		return invalid()
	}
	var tool, synthetic map[string]any
	if json.Unmarshal(chunks[0], &tool) != nil || json.Unmarshal(chunks[1], &synthetic) != nil {
		return invalid()
	}
	tc, sc := list(tool["candidates"]), list(synthetic["candidates"])
	if len(tc) != 1 || len(sc) != 1 {
		return invalid()
	}
	tp := list(object(object(tc[0])["content"])["parts"])
	sp := list(object(object(sc[0])["content"])["parts"])
	if len(tp) != 1 || len(sp) != 1 {
		return invalid()
	}
	call := object(object(tp[0])["functionCall"])
	thought := object(sp[0])
	if call == nil || len(thought) != 2 || thought["thought"] != true || thought["text"] != "" {
		return invalid()
	}
	id, _ := item["call_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		id, _ = item["id"].(string)
		id = strings.TrimSpace(id)
	}
	if id != "" && call["id"] != id {
		return invalid()
	}
	if name, _ := call["name"].(string); name == "" {
		return invalid()
	}
	if _, ok := call["args"].(map[string]any); !ok {
		return invalid()
	}
	return chunks[:1], nil
}
