// hostcheck exercises the pinned official CPA native host and C ABI.
// All credentials are fake; the only HTTP destination is this process's loopback server.
//
// Default mode requires safe direct ShutdownAll while a callback remains blocked.
// --disable checks the real ApplyConfig per-plugin disable path under that same
// active-request condition. Unpatched CPA v8.0.29 currently FAILS both: it marks
// the callback instance closed before native shutdown, rejecting the plugin's
// host.stream.close while an earlier host.stream.emit is still blocked. These
// assertions stay strict and evaluate before cleanup cancels the request.
//
// This is not a claim that normal CPA Service.Shutdown is broken: that lifecycle
// closes its HTTP server first, canceling request contexts before plugin unload.
// --cancel-first verifies that ordering against the same full native host bridge.
//
// --reenable disables an idle plugin, then opens the exact same library again
// and verifies fresh registration and requests, exercising a second native init.
//
// --reconfigure changes configuration on the same loaded library under active
// backpressure, then verifies old-stream retirement and fresh requests.
//
// --quiesce tests real version replacement, which invokes plugin.quiesce while
// callbacks still work, then safely unloads both libraries. No CPA source is patched.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

const checkTimeout = 3 * time.Second

var pluginID, modelID string

func main() {
	// A wedged native callback must fail CI rather than leave the harness hung.
	watchdog := time.AfterFunc(30*time.Second, func() {
		fmt.Fprintln(os.Stderr, "FAIL: native host integration exceeded 30 seconds")
		os.Exit(1)
	})
	defer watchdog.Stop()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// The public SDK Host wrapper does not expose executor registration. This
// standalone, version-pinned integration module uses CPA's real internal host,
// the same native loader and stream bridge that back that wrapper and CPA itself.
type executorRegistry struct{ executor coreauth.ProviderExecutor }

func (r *executorRegistry) Executor(provider string) (coreauth.ProviderExecutor, bool) {
	return r.executor, r.executor != nil && r.executor.Identifier() == provider
}
func (r *executorRegistry) RegisterExecutor(executor coreauth.ProviderExecutor) {
	r.executor = executor
}
func (r *executorRegistry) UnregisterExecutor(string) { r.executor = nil }

type mockUpstream struct {
	server         *httptest.Server
	failures       chan error
	closed         map[string]chan struct{}
	blockedWritten chan struct{}
	blockedOnce    sync.Once
	configCalls    atomic.Int32
	inferenceCalls atomic.Int32
}

func newMockUpstream() *mockUpstream {
	m := &mockUpstream{
		failures:       make(chan error, 16),
		closed:         map[string]chan struct{}{"execute": make(chan struct{}), "stream": make(chan struct{}), "blocked": make(chan struct{}), "fresh-execute": make(chan struct{}), "fresh-stream": make(chan struct{})},
		blockedWritten: make(chan struct{}),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	return m
}
func (m *mockUpstream) reject(w http.ResponseWriter, problem string) {
	select {
	case m.failures <- fmt.Errorf("mock upstream: %s", problem):
	default:
	}
	http.Error(w, problem, http.StatusBadRequest)
}
func (m *mockUpstream) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer hostcheck-fake-key" {
		m.reject(w, "wrong or missing fake service key")
		return
	}
	switch r.URL.Path {
	case "/console/api/v2/config":
		m.configCalls.Add(1)
		if r.Method != http.MethodGet {
			m.reject(w, "discovery must be GET")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"providers":{"p":{"package":"@opencode/ai/providers/openai/chat","settings":{"baseURL":%q},"models":{"m":{"compatibility":{"supportsStore":false},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh","max"]}],"limit":{"context":1048576,"input":524288,"output":131072},"capabilities":{"input":["text","image","video"],"output":["text"]}}}}}}`, m.server.URL+"/inference")
	case "/inference/chat/completions":
		m.inferenceCalls.Add(1)
		if r.Method != http.MethodPost {
			m.reject(w, "inference must be POST")
			return
		}
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil || body.Model != "m" || !body.Stream || len(body.Messages) != 1 {
			m.reject(w, "invalid native upstream model/messages/stream request")
			return
		}
		mode := body.Messages[0].Content
		for _, status := range []int{402, 403, 502} {
			if strings.HasPrefix(mode, fmt.Sprintf("error-%d-", status)) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", "hostcheck-private-request-id")
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"error_type":"server_error","message":"Synthetic diagnosis for status %d; hostcheck-fake-key; account_id=private-account"}}`, status)
				return
			}
		}
		closed, ok := m.closed[mode]
		if !ok {
			m.reject(w, "unknown request mode")
			return
		}
		defer close(closed)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flush := w.(http.Flusher)
		flush.Flush()
		if mode == "blocked" {
			// More than the actual host's 16-slot bridge plus its forwarding
			// goroutines can absorb without a downstream consumer.
			for i := 0; i < 256; i++ {
				if _, err := fmt.Fprintf(w, "data: {\"id\":\"blocked\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"chunk-%d\"},\"finish_reason\":null}]}\n\n", i); err != nil {
					return
				}
			}
			flush.Flush()
			m.blockedOnce.Do(func() { close(m.blockedWritten) })
		} else {
			io.WriteString(w, "data: {\"id\":\"native-host-check\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello native host\"},\"finish_reason\":null}]}\n\n")
			io.WriteString(w, "data: {\"id\":\"native-host-check\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			flush.Flush()
		}
		// Deliberately do not EOF after a terminal event. The plugin must stop
		// at [DONE], close the response body, and cancel this HTTP request.
		<-r.Context().Done()
	default:
		m.reject(w, "unexpected authenticated path: "+r.URL.Path)
	}
}

func run() error {
	args := os.Args[1:]
	mode := "shutdown"
	guard := false
	strictText := false
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch args[0] {
		case "--quiesce", "--disable", "--cancel-first", "--reconfigure", "--reenable":
			mode = strings.TrimPrefix(args[0], "--")
			args = args[1:]
		case "--strict-text":
			guard, strictText = true, true
			args = args[1:]
		case "--guard":
			guard = true
			args = args[1:]
		default:
			return fmt.Errorf("unknown mode %q", args[0])
		}
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: hostcheck [--quiesce|--disable|--cancel-first|--reconfigure|--reenable] [--guard|--strict-text] /absolute/plugin/directory")
	}
	directory, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	release, err := loadReleaseMetadata(directory)
	if err != nil {
		return err
	}
	pluginID, modelID = release.Provider, release.Provider+"/p/m"
	replacementFilename, err := release.replacementFilename()
	if err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "opencode-hostcheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	// Never let the host's version-selection cleanup mutate the build directory.
	pluginDirectory := filepath.Join(temporary, "plugins")
	if err := os.Mkdir(pluginDirectory, 0700); err != nil {
		return err
	}
	pluginBinary, err := os.ReadFile(filepath.Join(directory, release.Filename))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pluginDirectory, release.Filename), pluginBinary, 0700); err != nil {
		return err
	}
	catalog := filepath.Join(temporary, "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{}`), 0600); err != nil {
		return err
	}
	upstream := newMockUpstream()
	defer upstream.server.Close()
	var node yaml.Node
	pluginConfig := fmt.Sprintf("enabled: true\nallow_loopback_http: true\nrequest_timeout_seconds: 30\ncatalog_file: %q\nallowed_destinations:\n  - %q\n", catalog, upstream.server.URL)
	if guard {
		pluginConfig += fmt.Sprintf("isolated_test_guard:\n  enabled: true\n  strict_text_only: %t\n  allowed_requests:\n    - model: m\n      endpoint: %q\n", strictText, upstream.server.URL+"/inference/chat/completions")
	}
	if err := yaml.Unmarshal([]byte(pluginConfig), &node); err != nil {
		return err
	}
	enabled := true
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostConfig := &config.Config{AuthDir: temporary, Plugins: config.PluginsConfig{Enabled: true, Dir: pluginDirectory, Configs: map[string]config.PluginInstanceConfig{pluginID: {Enabled: &enabled, Raw: node}}}}
	// The old config subtree cannot enable the renamed plugin. No alias is
	// registered, avoiding duplicate auth/executor ownership during migration.
	legacyConfig := *hostConfig
	legacyConfig.Plugins = hostConfig.Plugins
	legacyConfig.Plugins.Configs = map[string]config.PluginInstanceConfig{"opencode-v2": {Enabled: &enabled, Raw: node}}
	host.ApplyConfig(ctx, &legacyConfig)
	if len(host.RegisteredPlugins()) != 0 {
		return fmt.Errorf("legacy config unexpectedly enabled renamed plugin")
	}
	host.ApplyConfig(ctx, hostConfig)
	plugins := host.RegisteredPlugins()
	if len(plugins) != 1 || plugins[0].ID != pluginID || plugins[0].Metadata.Version != release.Version {
		return fmt.Errorf("expected native plugin %s version %s, got %+v (the filename must be %s)", pluginID, release.Version, plugins, release.Filename)
	}
	if !host.HasAuthProvider(pluginID) {
		return fmt.Errorf("auth provider not registered")
	}
	raw, _ := json.Marshal(map[string]any{"type": pluginID, "key": "hostcheck-fake-key", "server": upstream.server.URL + "/console"})
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: pluginID, FileName: "fake.json", RawJSON: raw})
	if err != nil || !handled || auth == nil {
		return fmt.Errorf("auth parse failed: handled=%v err=%v", handled, err)
	}
	if auth.Provider != pluginID || host.HasAuthProvider("opencode-v2") {
		return fmt.Errorf("unexpected auth identity or legacy alias")
	}
	legacyRaw, _ := json.Marshal(map[string]any{"type": "opencode-v2", "key": "hostcheck-fake-key"})
	for _, provider := range []string{"opencode-v2", pluginID, ""} {
		_, legacyHandled, legacyErr := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "legacy.json", RawJSON: legacyRaw})
		if legacyErr != nil || legacyHandled {
			return fmt.Errorf("legacy auth must stay unhandled: provider=%q handled=%v err=%v", provider, legacyHandled, legacyErr)
		}
	}
	fmt.Println("PASS: old config cannot enable new plugin; legacy auth is unhandled; only opencode auth provider is registered")
	result := host.ModelsForAuth(ctx, auth)
	if result.Err != nil || !result.Handled || len(result.Models) != 1 || result.Models[0].ID != modelID {
		return fmt.Errorf("model discovery failed: handled=%v models=%d err=%v", result.Handled, len(result.Models), result.Err)
	}
	// Validate metadata through the actual C ABI and host adapter, rather than
	// only constructing a registry model inside a unit test.
	metadata := result.Models[0]
	if metadata.ContextLength != 1048576 || metadata.InputTokenLimit != 524288 || metadata.OutputTokenLimit != 131072 || metadata.MaxCompletionTokens != 131072 ||
		strings.Join(metadata.SupportedInputModalities, ",") != "text,image,video" || strings.Join(metadata.SupportedOutputModalities, ",") != "text" ||
		metadata.Thinking == nil || strings.Join(metadata.Thinking.Levels, ",") != "low,medium,high,xhigh,max" ||
		metadata.Thinking.Min != 0 || metadata.Thinking.Max != 0 || metadata.Thinking.ZeroAllowed || metadata.Thinking.DynamicAllowed {
		return fmt.Errorf("model capability metadata lost or invented across C ABI: %+v", metadata)
	}
	fmt.Println("PASS: catalog reasoning levels, exact token limits and modality declarations survive C ABI model discovery")
	_, handled, err = host.StartLogin(ctx, pluginID, "", nil)
	if !handled || err == nil || !strings.Contains(err.Error(), "OAuth") {
		return fmt.Errorf("OAuth login should explicitly return unsupported, got handled=%v err=%v", handled, err)
	}
	fmt.Println("PASS: C ABI load, registration, fake service-key parsing, mocked discovery, OAuth rejection")

	manager := &executorRegistry{}
	host.RegisterExecutors(manager, nil)
	executor, ok := manager.Executor(pluginID)
	if !ok {
		return fmt.Errorf("official host did not register native executor")
	}

	// Exercise actual plugin C ABI serialization and the official host's error
	// reconstruction, not just internal Go summary helpers. All values are fake.
	for _, status := range []int{402, 403, 502} {
		for _, streamed := range []bool{false, true} {
			id := fmt.Sprintf("error-%d-%t", status, streamed)
			request, options := requestFor(host, ctx, id, streamed)
			var err error
			if streamed {
				_, err = executor.ExecuteStream(ctx, auth, request, options)
			} else {
				_, err = executor.Execute(ctx, auth, request, options)
			}
			host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: id})
			var statusError interface{ StatusCode() int }
			if err == nil || !errors.As(err, &statusError) || statusError.StatusCode() != status {
				return fmt.Errorf("native error status lost for %s: %v", id, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("OpenCode upstream HTTP %d", status)) || !strings.Contains(err.Error(), fmt.Sprintf("Synthetic diagnosis for status %d", status)) || !strings.Contains(err.Error(), "[redacted]") {
				return fmt.Errorf("native error detail lost for %s: %v", id, err)
			}
			for _, secret := range []string{"hostcheck-fake-key", "private-account", "hostcheck-private-request-id"} {
				if strings.Contains(err.Error(), secret) {
					return fmt.Errorf("native error leaked synthetic private value for %s", id)
				}
			}
		}
	}
	fmt.Println("PASS: actual native host 402/403/502 aggregate and stream errors preserve upstream status and redacted message")

	request, options := requestFor(host, ctx, "execute", false)
	type executionResult struct {
		response coreexecutor.Response
		err      error
	}
	executed := make(chan executionResult, 1)
	go func() {
		response, err := executor.Execute(ctx, auth, request, options)
		executed <- executionResult{response, err}
	}()
	select {
	case result := <-executed:
		if result.err != nil {
			return fmt.Errorf("native execute: %w", result.err)
		}
		var body struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(result.response.Payload, &body) != nil || len(body.Choices) != 1 || body.Choices[0].Message.Content != "Hello native host" || body.Choices[0].FinishReason != "stop" || body.Usage.TotalTokens != 7 {
			return fmt.Errorf("native aggregation lost content, finish reason, or usage: %s", result.response.Payload)
		}
	case <-time.After(checkTimeout):
		return fmt.Errorf("native execute waited for EOF after terminal SSE")
	}
	if err := await(upstream.closed["execute"], "aggregated upstream response body closure"); err != nil {
		return err
	}
	host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "execute"})
	fmt.Println("PASS: actual native executor aggregation preserves content, finish reason, usage, and ends before upstream EOF")

	request, options = requestFor(host, ctx, "stream", true)
	stream, err := executor.ExecuteStream(ctx, auth, request, options)
	if err != nil {
		return fmt.Errorf("native stream start: %w", err)
	}
	streamDeadline := time.NewTimer(checkTimeout)
	defer streamDeadline.Stop()
	var received bytes.Buffer
streamLoop:
	for {
		select {
		case chunk, open := <-stream.Chunks:
			if !open {
				break streamLoop
			}
			if chunk.Err != nil {
				return fmt.Errorf("native stream chunk: %w", chunk.Err)
			}
			if !json.Valid(chunk.Payload) {
				return fmt.Errorf("Chat executor chunk is not JSON: %q", chunk.Payload)
			}
			received.Write(chunk.Payload)
		case <-streamDeadline.C:
			return fmt.Errorf("native stream waited for EOF after terminal SSE")
		}
	}
	for _, marker := range []string{"Hello native host", `"total_tokens":7`, `"finish_reason":"stop"`} {
		if !strings.Contains(received.String(), marker) {
			return fmt.Errorf("native stream missing %q: %s", marker, received.String())
		}
	}
	if err := await(upstream.closed["stream"], "streaming upstream response body closure"); err != nil {
		return err
	}
	host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "stream"})
	fmt.Println("PASS: native host streaming callbacks preserve JSON and terminate promptly on upstream [DONE] with upstream kept open")

	if mode == "reenable" {
		// All initial requests have completed. Do not exercise the separate
		// known host disable defect with an active blocked stream in this mode.
		return verifyDisableEnable(ctx, host, hostConfig, upstream, release, raw)
	}

	request, options = requestFor(host, ctx, "blocked", true)
	blocked, err := executor.ExecuteStream(ctx, auth, request, options)
	if err != nil {
		return fmt.Errorf("blocked native stream start: %w", err)
	}
	if blocked == nil || blocked.Chunks == nil {
		return fmt.Errorf("blocked native stream missing channel")
	}
	// Do not read blocked.Chunks before the lifecycle action has returned. Only
	// --cancel-first cancels ctx before that action; other modes assert their
	// deadline and upstream retirement before any cleanup cancellation or drain.
	if err := await(upstream.blockedWritten, "more than 16 upstream frames"); err != nil {
		return err
	}
	if err := awaitBlockedHostCallback(); err != nil {
		return err
	}
	if mode == "quiesce" {
		// CPA calls plugin.quiesce only when replacing a version. A second
		// temporary copy of the same test binary exercises that real path.
		if err := os.WriteFile(filepath.Join(pluginDirectory, replacementFilename), pluginBinary, 0700); err != nil {
			return err
		}
		replaced := make(chan struct{})
		go func() { host.ApplyConfig(ctx, hostConfig); close(replaced) }()
		if err := await(replaced, "native hot-reload quiesce with blocked host.stream.emit"); err != nil {
			return err
		}
		if len(host.RegisteredPlugins()) != 1 {
			return fmt.Errorf("plugin was not re-registered after hot reload")
		}
		if err := await(upstream.closed["blocked"], "in-flight upstream cancellation during quiesce"); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("request context unexpectedly canceled before quiesce verification")
		}
		fmt.Println("PASS: real version-replacement plugin.quiesce closes a full 16-slot host bridge and joins blocked workers")
	}
	if mode == "reconfigure" {
		if ctx.Err() != nil {
			return fmt.Errorf("caller context canceled before reconfigure")
		}
		select {
		case <-upstream.closed["blocked"]:
			return fmt.Errorf("old upstream request ended before reconfigure")
		default:
		}
		before, err := os.Stat(filepath.Join(pluginDirectory, release.Filename))
		if err != nil {
			return err
		}
		// A different timeout proves a real config update, with exactly the same
		// native path/version. No replacement library or pre-quiesce is involved.
		var updatedNode yaml.Node
		updatedYAML := strings.Replace(pluginConfig, "request_timeout_seconds: 30", "request_timeout_seconds: 31", 1)
		if err := yaml.Unmarshal([]byte(updatedYAML), &updatedNode); err != nil {
			return err
		}
		updatedConfig := *hostConfig
		updatedConfig.Plugins = hostConfig.Plugins
		updatedConfig.Plugins.Configs = map[string]config.PluginInstanceConfig{pluginID: {Enabled: &enabled, Raw: updatedNode}}
		reconfigured := make(chan struct{})
		go func() { host.ApplyConfig(ctx, &updatedConfig); close(reconfigured) }()
		if err := await(reconfigured, "same-instance reconfigure with blocked host.stream.emit"); err != nil {
			return err
		}
		if err := await(upstream.closed["blocked"], "old upstream cancellation during same-instance reconfigure"); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("caller context canceled before reconfigure retirement verification")
		}
		after, err := os.Stat(filepath.Join(pluginDirectory, release.Filename))
		if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("reconfigure changed the native library file: %v", err)
		}
		plugins := host.RegisteredPlugins()
		if len(plugins) != 1 || plugins[0].ID != pluginID || plugins[0].Metadata.Version != release.Version {
			return fmt.Errorf("same-instance registration changed after reconfigure: %+v", plugins)
		}
		// Drain only after ApplyConfig and upstream retirement have succeeded.
		// This checks the old terminal notification without relieving the
		// backpressure that the lifecycle transition was required to handle.
		if err := verifyRetiredStream(blocked); err != nil {
			return err
		}
		host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "blocked"})
		models := host.ModelsForAuth(ctx, auth)
		if models.Err != nil || !models.Handled || len(models.Models) != 1 || models.Models[0].ID != modelID {
			return fmt.Errorf("fresh discovery after reconfigure failed: %+v", models)
		}
		if err := verifyFreshRequests(ctx, host, executor, auth, upstream); err != nil {
			return err
		}
		fmt.Println("PASS: same-instance ApplyConfig retires a blocked stream without canceling the caller; fresh discovery, aggregate and streaming requests succeed")
	}
	if mode == "cancel-first" {
		// Match Service.Shutdown's ordering: HTTP request cancellation precedes
		// native unload. No downstream consumption is needed to unblock the bridge.
		cancel()
	}
	action := "direct native ShutdownAll with an active blocked stream"
	finished := make(chan struct{})
	if mode == "disable" {
		disabled := false
		disabledConfig := *hostConfig
		disabledConfig.Plugins = config.PluginsConfig{
			Enabled: true, Dir: pluginDirectory,
			Configs: map[string]config.PluginInstanceConfig{pluginID: {Enabled: &disabled, Raw: node}},
		}
		action = "ApplyConfig per-plugin disable with an active blocked stream"
		go func() { host.ApplyConfig(ctx, &disabledConfig); close(finished) }()
	} else {
		if mode == "cancel-first" {
			action = "native ShutdownAll after request cancellation"
		}
		if mode == "quiesce" {
			action = "native ShutdownAll after version-replacement quiesce"
		}
		if mode == "reconfigure" {
			action = "native ShutdownAll after same-instance reconfigure and fresh requests"
		}
		go func() { host.ShutdownAll(); close(finished) }()
	}
	if err := await(finished, action); err != nil {
		if mode == "shutdown" || mode == "disable" {
			if ctx.Err() != nil {
				return fmt.Errorf("request context was canceled before %s deadline", action)
			}
			return fmt.Errorf("%w; request context is still alive. Pinned CPA v8.0.29 disables callbacks before native unload and rejects host.stream.close; --cancel-first and --quiesce verify the safe lifecycle orderings", err)
		}
		return err
	}
	if err := await(upstream.closed["blocked"], "in-flight upstream request cancellation during "+action); err != nil {
		return err
	}
	if len(host.RegisteredPlugins()) != 0 || host.PluginBusy(pluginID) {
		return fmt.Errorf("native plugin remained loaded after %s", action)
	}
	if mode != "cancel-first" && ctx.Err() != nil {
		return fmt.Errorf("request context unexpectedly canceled before %s verification", action)
	}
	if mode == "cancel-first" && ctx.Err() == nil {
		return fmt.Errorf("request context was not canceled before native unload")
	}
	wantConfig, wantInference := int32(3), int32(9)
	if mode == "reconfigure" {
		wantConfig, wantInference = 4, 11
	}
	if upstream.configCalls.Load() != wantConfig || upstream.inferenceCalls.Load() != wantInference {
		return fmt.Errorf("unexpected mock calls: discovery=%d inference=%d", upstream.configCalls.Load(), upstream.inferenceCalls.Load())
	}
	select {
	case err := <-upstream.failures:
		return err
	default:
	}
	switch mode {
	case "reconfigure":
		fmt.Println("PASS: reconfigured native library unloaded cleanly after new requests completed")
	case "quiesce":
		fmt.Println("PASS: replacement and retired native libraries unloaded cleanly after quiesce")
	case "cancel-first":
		fmt.Println("PASS: request cancellation then native shutdown unblocks the full bridge and unloads cleanly (normal service shutdown ordering)")
	case "disable":
		fmt.Println("PASS: config disables plugin with blocked downstream and uncanceled request context")
	default:
		fmt.Println("PASS: full 16-slot official stream bridge, blocked downstream, uncanceled request context, native shutdown, and upstream cancellation")
	}
	return nil
}

func requestFor(host *pluginhost.Host, ctx context.Context, id string, stream bool) (coreexecutor.Request, coreexecutor.Options) {
	payload, _ := json.Marshal(map[string]any{"model": modelID, "stream": stream, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": id}}})
	intercept := host.InterceptRequestBeforeAuth(ctx, pluginapi.RequestInterceptRequest{RequestID: id, Model: modelID, RequestedModel: modelID})
	return coreexecutor.Request{Model: modelID, Payload: payload, Format: translator.FormatOpenAI}, coreexecutor.Options{Stream: stream, SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI, OriginalRequest: payload, Headers: intercept.Headers}
}
func await(done <-chan struct{}, description string) error {
	select {
	case <-done:
		return nil
	case <-time.After(checkTimeout):
		return fmt.Errorf("timeout waiting for %s", description)
	}
}
func awaitBlockedHostCallback() error {
	deadline := time.Now().Add(checkTimeout)
	for time.Now().Before(deadline) {
		stacks := make([]byte, 2<<20)
		n := runtime.Stack(stacks, true)
		if bytes.Contains(stacks[:n], []byte("pluginhost.(*streamBridgeStream).emit")) {
			// With no consumer, this callback cannot drain. Verify it is still
			// blocked after another scheduler interval, rather than sampling a transient emit.
			time.Sleep(50 * time.Millisecond)
			n = runtime.Stack(stacks, true)
			if bytes.Contains(stacks[:n], []byte("pluginhost.(*streamBridgeStream).emit")) {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("did not observe a blocked callback in the official native host stream bridge")
}
