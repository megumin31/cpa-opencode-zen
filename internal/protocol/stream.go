package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/megumin31/cpa-opencode-zen/internal/diagnostics"
	"github.com/megumin31/cpa-opencode-zen/internal/fault"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

var errTerminal = errors.New("terminal event")

type sseEvent struct {
	Event string
	Data  []byte
	ID    string
	IDSet bool
}

func (e sseEvent) wire() []byte {
	var b bytes.Buffer
	if e.Event != "" {
		b.WriteString("event: " + e.Event + "\n")
	}
	if e.IDSet || e.ID != "" {
		b.WriteString("id: " + e.ID + "\n")
	}
	for _, line := range bytes.Split(e.Data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// readSSE handles arbitrary fragmentation and CR, LF and CRLF delimiters.
// Like the native parser, only a blank line dispatches an event; pending data
// at EOF is discarded. Last-event ID survives event boundaries, including reset.
// Keep the plugin's 32 MiB byte bound (and aggregate bound below), rather than
// copying the native parser's 10 MiB JavaScript pending-string character bound.
func readSSE(ctx context.Context, r io.Reader, visit func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxResponseBytes)
	// A CR completes the line immediately, even if the following LF has not
	// arrived. Waiting for that LF would block a definitive terminal event.
	skipLF := false
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		offset := 0
		if skipLF && len(data) > 0 {
			skipLF = false
			if data[0] == '\n' {
				offset = 1
				data = data[1:]
			}
		}
		for i, b := range data {
			if b == '\r' || b == '\n' {
				skipLF = b == '\r'
				return offset + i + 1, data[:i], nil
			}
		}
		if atEOF {
			return offset + len(data), nil, nil
		}
		return offset, nil, nil
	})
	event := sseEvent{}
	var data []string
	size := 0
	first := true
	dispatch := func() error {
		if len(data) == 0 {
			event = sseEvent{ID: event.ID, IDSet: event.IDSet}
			size = 0
			return nil
		}
		event.Data = []byte(strings.Join(data, "\n"))
		err := visit(event)
		event = sseEvent{ID: event.ID, IDSet: event.IDSet}
		data = nil
		size = 0
		return err
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return contextFailure(ctx)
		}
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		size += len(line) + 1
		if size > maxResponseBytes {
			return fault.New(502, "upstream SSE event exceeds size limit")
		}
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			event.Event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				event.ID = value
				event.IDSet = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return contextFailure(ctx)
		}
		var status *fault.Error
		if errors.As(err, &status) {
			return status
		}
		return fault.New(502, "upstream event stream was interrupted")
	}
	if ctx.Err() != nil {
		return contextFailure(ctx)
	}
	return nil
}
func eventObject(ctx context.Context, e sseEvent) (map[string]any, error) {
	if e.Event == "error" || e.Event == "response.failed" {
		return nil, diagnostics.SSEError(ctx, e.Data)
	}
	var obj map[string]any
	if json.Unmarshal(e.Data, &obj) != nil || obj == nil {
		return nil, diagnostics.SSEError(ctx, e.Data)
	}
	if _, ok := obj["error"]; ok {
		return nil, diagnostics.SSEError(ctx, e.Data)
	}
	typ, _ := obj["type"].(string)
	if typ == "error" || typ == "response.failed" {
		return nil, diagnostics.SSEError(ctx, e.Data)
	}
	return obj, nil
}
func terminal(protocol string, e sseEvent, obj map[string]any) bool {
	switch protocol {
	case "openai":
		return bytes.Equal(bytes.TrimSpace(e.Data), []byte("[DONE]"))
	case "openai-response":
		t, _ := obj["type"].(string)
		return t == "response.completed" || t == "response.incomplete"
	case "claude":
		return obj["type"] == "message_stop"
	case "gemini":
		for _, c := range list(obj["candidates"]) {
			m := object(c)
			if s, _ := m["finishReason"].(string); s != "" {
				return true
			}
		}
		reason, _ := object(obj["promptFeedback"])["blockReason"].(string)
		return reason != "" && reason != "BLOCK_REASON_UNSPECIFIED"
	}
	return false
}

// Native framing discards these exact payloads before protocol decoding.
func ignoredStreamEvent(protocol string, e sseEvent) bool {
	data := string(e.Data)
	return data == "" || data == "null" || data == ": keepalive" ||
		(protocol != "openai" && data == "[DONE]")
}

type streamCompletion struct {
	protocol            string
	requireFinishReason bool
	finished            bool
	done                bool
	choices             map[int]bool
	metadata            map[string]any
}

func newStreamCompletion(protocol string, routes ...catalog.Route) *streamCompletion {
	c := &streamCompletion{protocol: protocol, requireFinishReason: true, choices: map[int]bool{}, metadata: map[string]any{}}
	if len(routes) > 0 {
		if value, ok := routes[0].Compatibility["requireFinishReason"].(bool); ok {
			c.requireFinishReason = value
		}
	}
	return c
}

func (c *streamCompletion) observe(e sseEvent, obj map[string]any) error {
	if c.protocol == "openai" {
		for _, key := range []string{"id", "model", "created", "system_fingerprint", "service_tier"} {
			if value := obj[key]; value != nil {
				c.metadata[key] = value
			}
		}
		for _, value := range list(obj["choices"]) {
			choice := object(value)
			index := int(integer(choice["index"]))
			delta := object(choice["delta"])
			c.choices[index] = c.choices[index] || len(list(delta["tool_calls"])) > 0 || delta["function_call"] != nil
			reason := object(value)["finish_reason"]
			if reason == nil || reason == "" {
				continue
			}
			switch reason {
			case "stop", "end", "length", "content_filter", "function_call", "tool_calls":
				c.finished = true
			case "error":
				return fault.New(502, "upstream reported a generation error finish_reason")
			case "network_error":
				return fault.New(502, "upstream reported a network error finish_reason")
			default:
				return fault.New(502, "upstream reported an unsupported finish_reason")
			}
		}
	}
	if terminal(c.protocol, e, obj) {
		c.done = true
		return c.validate()
	}
	return nil
}

// With the explicit override, native onHalt still emits a normalized finish.
// Supply that final choice to both passthrough clients and SDK translators.
func (c *streamCompletion) fallback() map[string]any {
	if c.protocol != "openai" || c.requireFinishReason || c.finished {
		return nil
	}
	if len(c.choices) == 0 {
		c.choices[0] = false
	}
	choices := []any{}
	for _, index := range sortedIndexes(c.choices) {
		reason := "stop"
		if c.choices[index] {
			reason = "tool_calls"
		}
		choices = append(choices, map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": reason})
	}
	result := catalog.Merge(nil, c.metadata)
	result["object"] = "chat.completion.chunk"
	result["choices"] = choices
	return result
}

func (c *streamCompletion) validate() error {
	if c.protocol == "openai" {
		if c.requireFinishReason && !c.finished {
			return fault.New(502, "upstream Chat stream ended without finish_reason")
		}
		return nil
	}
	if !c.done {
		return fault.New(502, "upstream stream ended before a terminal event")
	}
	return nil
}

// Forward validates and translates one native stream into the requested output.
// It retains terminal-event, completion, size-limit, and downstream-close checks.
// Callers should preflight ConversionSupported before opening an upstream request.
func Forward(ctx context.Context, r io.Reader, route catalog.Route, output translator.Format, original, request []byte, emit func([]byte) error) error {
	up := FormatName(route.Protocol)
	if !ConversionAllowed(up, output) {
		return fault.New(400, "Claude-to-Gemini response conversion is not supported safely by the pinned SDK")
	}
	if !ConversionSupported(up, output, true) {
		return fault.New(400, "requested response protocol conversion is unavailable")
	}
	var originalObject map[string]any
	if json.Unmarshal(original, &originalObject) == nil && originalObject != nil {
		originalObject["stream"] = true
		original, _ = json.Marshal(originalObject)
	}
	var param any
	completion := newStreamCompletion(route.Protocol, route)
	var process func(sseEvent) error
	process = func(e sseEvent) error {
		if ignoredStreamEvent(route.Protocol, e) {
			return nil
		}
		var obj map[string]any
		var err error
		if !bytes.Equal(bytes.TrimSpace(e.Data), []byte("[DONE]")) {
			obj, err = eventObject(ctx, e)
			if err != nil {
				return err
			}
		}
		if up == translator.FormatGemini && output != up {
			if reason, _ := object(obj["promptFeedback"])["blockReason"].(string); reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
				return fault.New(502, "upstream Gemini blocked the response; refusal cannot be safely converted")
			}
		}
		if terminal(route.Protocol, e, obj) {
			if fallback := completion.fallback(); fallback != nil {
				data, _ := json.Marshal(fallback)
				if err := process(sseEvent{Data: data}); err != nil {
					return err
				}
			}
		}
		if err := completion.observe(e, obj); err != nil {
			return err
		}
		if up == output {
			if err := emit(e.wire()); err != nil {
				return fault.New(499, "downstream stream closed")
			}
			if completion.done && route.Protocol != "gemini" {
				return errTerminal
			}
			return nil
		}
		// CPA built-ins consume one normalized data record at a time. Preserve the
		// complete JSON payload for multiline SSE records rather than splitting it.
		normalized, err := sdkResponseBody(route, e.Data)
		if err != nil {
			return err
		}
		data := append([]byte("data: "), bytes.TrimSpace(normalized)...)
		var geminiUsage []byte
		if up == translator.FormatOpenAI && output == translator.FormatGemini {
			geminiUsage, err = prepareChatGeminiStream(obj)
			if err != nil {
				return err
			}
		}
		var chunks [][]byte
		for _, record := range translationRecords(up, output, data) {
			chunks = append(chunks, registry.TranslateStream(ctx, sdkUpstream(up, output), output, route.Alias, original, request, record, &param)...)
		}
		if err := translationError(param); err != nil {
			return err
		}
		if len(geminiUsage) > 0 {
			chunks = append(chunks, geminiUsage)
		}
		if up == translator.FormatOpenAIResponse && output == translator.FormatGemini {
			if obj["type"] == "response.output_item.done" && object(obj["item"])["type"] == "function_call" {
				if len(chunks) != 0 {
					return fault.New(502, "unexpected Gemini tool converter output")
				}
				// Drain the pinned converter's single pending-tool slot immediately.
				// Empty reasoning does not mark text as delivered; discard only its
				// verified synthetic record, retaining SDK name/argument/id mapping.
				drained := registry.TranslateStream(ctx, sdkUpstream(up, output), output, route.Alias, original, request, []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":""}`), &param)
				chunks, err = drainedResponsesGeminiTool(drained, object(obj["item"]))
				if err != nil {
					return err
				}
			}
			eventType, _ := obj["type"].(string)
			chunks, err = completeResponsesGemini(chunks, eventType)
			if err != nil {
				return err
			}
		}
		for _, chunk := range chunks {
			if output == translator.FormatGemini && ctx.Err() != nil {
				return contextFailure(ctx)
			}
			if wire := streamFrame(chunk); len(wire) > 0 {
				if err := emit(wire); err != nil {
					return fault.New(499, "downstream stream closed")
				}
			}
		}
		if completion.done && route.Protocol != "gemini" {
			return errTerminal
		}
		return nil
	}
	err := readSSE(ctx, r, process)
	if errors.Is(err, errTerminal) {
		err = nil
	}
	if err != nil {
		return err
	}
	if err := completion.validate(); err != nil {
		return err
	}
	// Clean EOF is a valid native Chat halt after a finish reason (or with the
	// explicit compatibility override). Flush terminal-driven SDK converters.
	if route.Protocol == "openai" && !completion.done {
		if err := process(sseEvent{Data: []byte("[DONE]")}); err != nil && !errors.Is(err, errTerminal) {
			return err
		}
	}
	// Gemini has no wire [DONE] sentinel. The pinned Gemini-to-Claude SDK
	// converter nevertheless requires an explicit flush to emit message_stop.
	// Only flush after clean EOF and validated finishReason, so
	// truncated or failed streams cannot become apparent successful completions.
	if up == translator.FormatGemini && output == translator.FormatClaude {
		chunks := registry.TranslateStream(ctx, up, output, route.Alias, original, request, []byte("[DONE]"), &param)
		if err := translationError(param); err != nil {
			return err
		}
		for _, chunk := range chunks {
			if wire := streamFrame(chunk); len(wire) > 0 {
				if err := emit(wire); err != nil {
					return fault.New(499, "downstream stream closed")
				}
			}
		}
	}
	if up != output && output == translator.FormatOpenAI {
		return emit([]byte("data: [DONE]\n\n"))
	}
	return nil
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any            { a, _ := v.([]any); return a }
func appendText(m map[string]any, key string, value any) {
	s, ok := value.(string)
	if !ok {
		return
	}
	before, _ := m[key].(string)
	m[key] = before + s
}

// Aggregate validates and combines one bounded native event stream.
// Optional route compatibility controls native completion requirements.
func Aggregate(ctx context.Context, r io.Reader, protocol string, routes ...catalog.Route) ([]byte, error) {
	a := &streamAggregate{protocol: protocol, result: map[string]any{}, choices: map[int]map[string]any{}, tools: map[int]map[int]map[string]any{}, blocks: map[int]map[string]any{}, inputs: map[int]string{}, candidates: map[int]map[string]any{}}
	completion := newStreamCompletion(protocol, routes...)
	err := readSSE(ctx, r, func(e sseEvent) error {
		if ignoredStreamEvent(protocol, e) {
			return nil
		}
		a.size += len(e.Data)
		if a.size > maxResponseBytes {
			return fault.New(502, "aggregated response exceeds size limit")
		}
		var o map[string]any
		var err error
		if !bytes.Equal(bytes.TrimSpace(e.Data), []byte("[DONE]")) {
			o, err = eventObject(ctx, e)
			if err != nil {
				return err
			}
		}
		if err := completion.observe(e, o); err != nil {
			return err
		}
		if o != nil {
			if err := a.add(o); err != nil {
				return err
			}
		}
		if completion.done && protocol != "gemini" {
			return errTerminal
		}
		return nil
	})
	if errors.Is(err, errTerminal) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if err := completion.validate(); err != nil {
		return nil, err
	}
	if fallback := completion.fallback(); fallback != nil {
		if err := a.add(fallback); err != nil {
			return nil, err
		}
	}
	return a.finish()
}

type streamAggregate struct {
	protocol   string
	result     map[string]any
	size       int
	choices    map[int]map[string]any
	tools      map[int]map[int]map[string]any
	blocks     map[int]map[string]any
	inputs     map[int]string
	candidates map[int]map[string]any
	responses  *responsesAggregate
}

func (a *streamAggregate) add(o map[string]any) error {
	switch a.protocol {
	case "openai":
		for _, k := range []string{"id", "created", "model", "system_fingerprint", "service_tier", "usage"} {
			if v, ok := o[k]; ok && v != nil {
				a.result[k] = catalog.Clone(v)
			}
		}
		a.result["object"] = "chat.completion"
		for _, v := range list(o["choices"]) {
			choice := object(v)
			index := int(integer(choice["index"]))
			dst := a.choices[index]
			if dst == nil {
				dst = map[string]any{"index": index, "message": map[string]any{"role": "assistant"}}
				a.choices[index] = dst
			}
			for _, k := range []string{"finish_reason", "logprobs"} {
				if value, ok := choice[k]; ok && value != nil {
					dst[k] = catalog.Clone(value)
				}
			}
			delta := object(choice["delta"])
			message := object(dst["message"])
			for k, value := range delta {
				switch k {
				case "tool_calls":
					if a.tools[index] == nil {
						a.tools[index] = map[int]map[string]any{}
					}
					for _, tv := range list(value) {
						tool := object(tv)
						ti := int(integer(tool["index"]))
						t := a.tools[index][ti]
						if t == nil {
							t = map[string]any{"function": map[string]any{}}
							a.tools[index][ti] = t
						}
						for _, name := range []string{"id", "type"} {
							if x, ok := tool[name]; ok {
								t[name] = x
							}
						}
						for key, x := range object(tool["function"]) {
							appendText(object(t["function"]), key, x)
						}
					}
				case "reasoning_details":
					message[k] = append(list(message[k]), list(catalog.Clone(value))...)
				case "content", "reasoning_content", "reasoning", "reasoning_text", "refusal":
					if _, ok := value.(string); ok {
						appendText(message, k, value)
					} else if value != nil {
						message[k] = catalog.Clone(value)
					}
				default:
					if value != nil {
						message[k] = catalog.Clone(value)
					}
				}
			}
		}
	case "claude":
		switch o["type"] {
		case "message_start":
			a.result = catalog.Merge(nil, object(o["message"]))
		case "content_block_start":
			index := int(integer(o["index"]))
			a.blocks[index] = catalog.Merge(nil, object(o["content_block"]))
		case "content_block_delta":
			index := int(integer(o["index"]))
			block := a.blocks[index]
			if block == nil {
				return fault.New(502, "Anthropic delta preceded block start")
			}
			delta := object(o["delta"])
			switch delta["type"] {
			case "text_delta":
				appendText(block, "text", delta["text"])
			case "thinking_delta":
				appendText(block, "thinking", delta["thinking"])
			case "signature_delta":
				appendText(block, "signature", delta["signature"])
			case "input_json_delta":
				s, _ := delta["partial_json"].(string)
				a.inputs[index] += s
			}
		case "content_block_stop":
			index := int(integer(o["index"]))
			if raw := a.inputs[index]; raw != "" {
				var input any
				if json.Unmarshal([]byte(raw), &input) != nil {
					return fault.New(502, "Anthropic tool arguments are invalid JSON")
				}
				a.blocks[index]["input"] = input
			}
		case "message_delta":
			a.result = catalog.Merge(a.result, object(o["delta"]))
			if usage := object(o["usage"]); usage != nil {
				a.result["usage"] = catalog.Merge(object(a.result["usage"]), usage)
			}
		}
	case "openai-response":
		if a.responses == nil {
			a.responses = newResponsesAggregate()
		}
		return a.responses.add(o, a.result)
	case "gemini":
		for _, k := range []string{"usageMetadata", "modelVersion", "responseId", "promptFeedback"} {
			if v, ok := o[k]; ok {
				a.result[k] = catalog.Clone(v)
			}
		}
		for _, v := range list(o["candidates"]) {
			c := object(v)
			index := int(integer(c["index"]))
			dst := a.candidates[index]
			if dst == nil {
				dst = map[string]any{"index": index, "content": map[string]any{"role": "model", "parts": []any{}}}
				a.candidates[index] = dst
			}
			for k, v := range c {
				if k != "content" {
					dst[k] = catalog.Clone(v)
				}
			}
			content := object(dst["content"])
			parts := list(content["parts"])
			for _, p := range list(object(c["content"])["parts"]) {
				part := catalog.Merge(nil, object(p))
				if len(parts) > 0 && part["text"] != nil {
					previous := object(parts[len(parts)-1])
					if previous["text"] != nil && previous["thought"] == part["thought"] && part["thoughtSignature"] == nil && previous["thoughtSignature"] == nil {
						appendText(previous, "text", part["text"])
						continue
					}
				}
				parts = append(parts, part)
			}
			content["parts"] = parts
		}
	default:
		return fault.New(400, "unsupported aggregation protocol")
	}
	return nil
}
func sortedIndexes[T any](m map[int]T) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
func (a *streamAggregate) finish() ([]byte, error) {
	switch a.protocol {
	case "openai":
		choices := []any{}
		for _, index := range sortedIndexes(a.choices) {
			c := a.choices[index]
			if tools := a.tools[index]; len(tools) > 0 {
				items := []any{}
				for _, ti := range sortedIndexes(tools) {
					items = append(items, tools[ti])
				}
				object(c["message"])["tool_calls"] = items
			}
			choices = append(choices, c)
		}
		a.result["choices"] = choices
	case "claude":
		blocks := []any{}
		for _, i := range sortedIndexes(a.blocks) {
			if a.inputs[i] != "" {
				var input any
				if json.Unmarshal([]byte(a.inputs[i]), &input) != nil {
					return nil, fault.New(502, "invalid tool arguments")
				}
				a.blocks[i]["input"] = input
			}
			blocks = append(blocks, a.blocks[i])
		}
		a.result["content"] = blocks
	case "gemini":
		candidates := []any{}
		for _, i := range sortedIndexes(a.candidates) {
			candidates = append(candidates, a.candidates[i])
		}
		a.result["candidates"] = candidates
	case "openai-response":
		if a.responses != nil {
			a.result["output"] = a.responses.output()
		}
	}
	return json.Marshal(a.result)
}

func contextFailure(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fault.New(504, "upstream request deadline exceeded")
	}
	return fault.New(499, "request canceled")
}

func integer(v any) int64 {
	switch v := v.(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		i, _ := v.Int64()
		return i
	}
	return 0
}
