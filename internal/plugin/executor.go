package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/megumin31/cpa-opencode-zen/internal/diagnostics"
	"github.com/megumin31/cpa-opencode-zen/internal/protocol"
	"github.com/megumin31/cpa-opencode-zen/internal/transport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id"`
}

func (e *Engine) execute(raw []byte, stream bool) (any, error) {
	g, done, beginErr := e.begin()
	if beginErr != nil {
		return nil, beginErr
	}
	keep := false
	defer func() {
		if !keep {
			done()
		}
	}()
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail(400, "invalid executor request")
	}
	if stream && req.StreamID == "" {
		return nil, fail(400, "missing stream ID")
	}
	c, err := decodeCredential(g.config, req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return nil, err
	}
	ctx, cancel := executionContext(g, req.ExecutorRequest)
	defer func() {
		if !keep {
			cancel()
		}
	}()
	proxy := storedHostProxy(g, c)
	routes, err := e.discover(g, ctx, c, proxy, false)
	if err != nil {
		return nil, err
	}
	r, ok := routes[req.Model]
	if !ok && strings.HasPrefix(req.Model, "opencode/opencode/") {
		r, ok = routes["opencode-zen/"+strings.TrimPrefix(req.Model, "opencode/opencode/")]
	}
	if !ok {
		return nil, fail(404, "model is absent from the current OpenCode Console configuration")
	}
	if r.Unsupported != "" {
		return nil, fail(400, "model package or endpoint is unsupported")
	}
	input := protocol.FormatName(req.SourceFormat)
	if input == "" {
		input = protocol.FormatName(req.Format)
	}
	output := protocol.FormatName(req.Format)
	if output == "" {
		output = input
	}
	up := protocol.FormatName(r.Protocol)
	ctx, transportCancel, err := transport.WithOptions(ctx, r.Settings)
	if err != nil {
		return nil, err
	}
	executionCancel := cancel
	cancel = func() { transportCancel(); executionCancel() }
	affinity, err := e.sessions.headers(c, req.ExecutorRequest)
	if err != nil {
		return nil, err
	}
	payload, target, headers, err := e.prepareRequest(g.config, ctx, c, r, input, req.Payload, affinity.Get("X-Session-Affinity"))
	if err != nil {
		return nil, err
	}
	for k, v := range affinity {
		headers[k] = v
	}
	if !protocol.ConversionAllowed(up, output) {
		return nil, fail(400, "Claude-to-Gemini response conversion is not supported safely by the pinned SDK")
	}
	if !protocol.ConversionSupported(up, output, stream) {
		return nil, fail(400, "requested response protocol conversion is unavailable")
	}
	// Test-only restriction inspects the exact serialized payload and endpoint
	// after every native/config overlay. It never rewrites protocol semantics.
	guardConfig := g.config
	if guardConfig.TestGuard != nil && guardConfig.TestGuard.StrictTextOnly && (!strictTextHeaders(headers) || (r.Settings["encoding"] != nil && r.Settings["encoding"] != "none")) {
		e.logf("OpenCode isolated test guard: enabled=true allowed=false")
		return nil, fail(403, "isolated test guard rejected the final upstream model or endpoint")
	}
	if err := e.checkInferenceGuard(guardConfig, r.Protocol, target, payload); err != nil {
		return nil, err
	}
	cooldownScope, err := requestCooldownKey(c, r, payload)
	if err != nil {
		return nil, err
	}
	if err := e.cooldowns.check(cooldownScope); err != nil {
		return nil, err
	}
	resp, err := outbound(g, ctx, c, proxy, "POST", target, headers, payload, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e.cooldowns.rememberHeaders(cooldownScope, resp.StatusCode, resp.Header)
		diagnostic := diagnostics.CaptureHTTP(ctx, resp, append(diagnosticSecrets(c, headers, target, proxy, g.config.ProxyURL), g.config.WorkerToken))
		diagnostics.Log("upstream", diagnostic, e.logf)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			invalidate(g, c)
			_, _ = e.discover(g, ctx, c, proxy, true)
		}
		return nil, diagnostic.ClientError()
	}
	ctx = diagnostics.WithSSE(ctx, resp, append(diagnosticSecrets(c, headers, target, proxy, g.config.ProxyURL), g.config.WorkerToken), e.logf)
	// Native adapters parse the requested event stream independently of MIME labels.
	// The bounded SSE parser and protocol completion checks validate the body.
	if stream {
		g.Lock()
		if g.closing {
			g.Unlock()
			resp.Body.Close()
			return nil, fail(503, "plugin shutting down")
		}
		if _, exists := g.streams[req.StreamID]; exists {
			g.Unlock()
			resp.Body.Close()
			return nil, fail(409, "duplicate active stream ID")
		}
		g.streams[req.StreamID] = struct{}{}
		g.Unlock()
		keep = true
		// The host bridge callback has its own context. A request deadline or
		// request.complete must therefore close the bridge as well as cancel HTTP.
		// Join a triggered watcher before releasing the worker lease so ABI shutdown
		// cannot invalidate host pointers while that callback is still executing.
		watcherDone := make(chan struct{})
		stopWatcher := context.AfterFunc(ctx, func() {
			defer close(watcherDone)
			e.closeStream(g, req.StreamID, transport.Failure(ctx.Err()).Error())
		})
		go func() {
			defer done()
			defer cancel()
			defer resp.Body.Close()
			defer func() {
				if !stopWatcher() {
					<-watcherDone
				}
			}()
			err := protocol.Forward(ctx, resp.Body, r, output, req.Payload, payload, func(p []byte) error {
				if e.streams == nil {
					return fail(500, "host stream callback unavailable")
				}
				return e.streams.Emit(req.StreamID, p)
			})
			message := ""
			if err != nil {
				message = err.Error()
			}
			e.closeStream(g, req.StreamID, message)
		}()
		return map[string]any{"headers": responseHeaders(resp.Header, true)}, nil
	}
	defer resp.Body.Close()
	body, err := protocol.Aggregate(ctx, resp.Body, r.Protocol, r)
	if err != nil {
		return nil, err
	}
	body, err = protocol.ConvertResponse(ctx, r, output, req.Model, req.Payload, payload, body)
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, fail(502, "upstream response could not be translated")
	}
	return pluginapi.ExecutorResponse{Payload: body, Headers: responseHeaders(resp.Header, false)}, nil
}
func responseHeaders(in http.Header, stream bool) http.Header {
	out := http.Header{}
	for _, k := range []string{"Retry-After", "X-Request-Id", "Request-Id", "Openai-Processing-Ms"} {
		if v := in.Values(k); len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	if stream {
		out.Set("Content-Type", "text/event-stream")
	} else {
		out.Set("Content-Type", "application/json")
	}
	return out
}

// prepareRequest binds immutable execution policy to a pure protocol request.
// Credential material is added here, never passed to the codec package.
func (e *Engine) prepareRequest(cfg pluginConfig, ctx context.Context, c credential, r catalog.Route, source translator.Format, raw []byte, affinity ...string) ([]byte, string, http.Header, error) {
	if _, err := transport.ParseOptions(r.Settings); err != nil {
		return nil, "", nil, err
	}
	options := protocol.Options{Logf: e.logf, StrictTextOnly: cfg.TestGuard != nil && cfg.TestGuard.Enabled && cfg.TestGuard.StrictTextOnly}
	if len(affinity) > 0 {
		options.Affinity = affinity[0]
	}
	payload, target, headers, err := protocol.Prepare(ctx, r, source, raw, options)
	if err != nil {
		return nil, "", nil, err
	}
	// Recheck the final wire body after all configured overlays. Public
	// discovery verified the route's ModelID; no caller/variant may redirect it.
	if c.Key == "public" {
		var final map[string]any
		if json.Unmarshal(payload, &final) != nil {
			return nil, "", nil, fail(400, "invalid public inference body")
		}
		model, present := final["model"]
		if (r.Protocol != "gemini" && model != r.ModelID) || (r.Protocol == "gemini" && present && model != r.ModelID) {
			return nil, "", nil, fail(400, "public inference cannot override the discovered free model")
		}
	}
	if err := transport.AllowedURL(cfg.transportPolicy(), target); err != nil {
		return nil, "", nil, err
	}
	switch r.Protocol {
	case "openai", "openai-response":
		headers.Set("Authorization", "Bearer "+c.Key)
	case "claude":
		headers.Set("x-api-key", c.Key)
	case "gemini":
		headers.Set("x-goog-api-key", c.Key)
	}
	return payload, target, headers, nil
}
func outbound(g *generation, ctx context.Context, c credential, hostProxy, method, target string, headers http.Header, body []byte, authenticated bool) (*http.Response, error) {
	return g.transport.Do(ctx, g.config.transportPolicy(), c.ProxyURL, hostProxy, method, target, headers, body, authenticated)
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any            { a, _ := v.([]any); return a }
