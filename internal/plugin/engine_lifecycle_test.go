package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/megumin31/cpa-opencode-zen/internal/fault"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// These fixtures own all dependencies and never use the compatibility singleton
// or real network. The reserved URL is handled only by the injected transport.
const engineFixtureOrigin = "https://engine-test.invalid"

func engineFixtureJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func engineFixtureConfig(t *testing.T, timeout int) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(file, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("catalog_file: %q\nallowed_destinations:\n  - %q\nrequest_timeout_seconds: %d\n", file, engineFixtureOrigin, timeout)
	return engineFixtureJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte(cfg)})
}

func engineFixtureCredential() credential {
	return credential{Type: providerID, Key: "synthetic-engine-key", Server: engineFixtureOrigin + "/console", OrgID: "synthetic-org"}
}

func engineFixtureGeneration(t *testing.T, e *Engine) *generation {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		t.Fatal("engine has no published generation")
	}
	return e.current
}

func engineFixtureWait(t *testing.T, ready <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func engineFixtureStatus(t *testing.T, err error, want int) {
	t.Helper()
	var status *fault.Error
	if !errors.As(err, &status) || status.Status != want {
		t.Fatalf("status=%v, want %d", err, want)
	}
}

func engineFixtureNoopLog(string, ...any) {}

type engineFixtureTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
	requests  atomic.Int64
	closes    atomic.Int64
}

func (r *engineFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests.Add(1)
	return r.roundTrip(req)
}
func (r *engineFixtureTransport) CloseIdleConnections() { r.closes.Add(1) }

func engineFixtureResponse(req *http.Request, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: req}
}
func engineFixtureConsole(req *http.Request, model string, overrides ...map[string]any) *http.Response {
	settings := map[string]any{"baseURL": engineFixtureOrigin + "/inference/v1"}
	for _, values := range overrides {
		for key, value := range values {
			settings[key] = value
		}
	}
	rawSettings, _ := json.Marshal(settings)
	body := fmt.Sprintf(`{"providers":{"p":{"package":"@opencode/ai/providers/openai/chat","settings":%s,"models":{"m":{"modelID":%q}}}}}`, rawSettings, model)
	return engineFixtureResponse(req, io.NopCloser(strings.NewReader(body)))
}

// Close releases the blocked Emit, but Emit cannot return until finish is
// opened. This proves retirement waits for actual worker exit after closing the
// host bridge, rather than mistaking cancellation/Close for a completed join.
type engineBlockedHost struct {
	closeFinish     chan struct{}
	closeExited     chan struct{}
	closeFinishOnce sync.Once
	closeExitOnce   sync.Once
	started         chan struct{}
	released        chan struct{}
	finish          chan struct{}
	exited          chan struct{}
	startOnce       sync.Once
	releaseOnce     sync.Once
	finishOnce      sync.Once
	exitOnce        sync.Once
	closes          atomic.Int64
	mu              sync.Mutex
	ids             []string
	messages        []string
}

func newEngineBlockedHost() *engineBlockedHost {
	return &engineBlockedHost{started: make(chan struct{}), released: make(chan struct{}), finish: make(chan struct{}), exited: make(chan struct{}), closeExited: make(chan struct{})}
}
func (h *engineBlockedHost) Emit(string, []byte) error {
	h.startOnce.Do(func() { close(h.started) })
	<-h.released
	<-h.finish
	h.exitOnce.Do(func() { close(h.exited) })
	return errors.New("synthetic downstream closed")
}
func (h *engineBlockedHost) Close(id, message string) error {
	h.closes.Add(1)
	h.mu.Lock()
	h.ids = append(h.ids, id)
	h.messages = append(h.messages, message)
	h.mu.Unlock()
	h.releaseOnce.Do(func() { close(h.released) })
	if h.closeFinish != nil {
		<-h.closeFinish
	}
	h.closeExitOnce.Do(func() { close(h.closeExited) })
	return nil
}
func (h *engineBlockedHost) unblock() {
	h.releaseOnce.Do(func() { close(h.released) })
	h.finishOnce.Do(func() { close(h.finish) })
	if h.closeFinish != nil {
		h.closeFinishOnce.Do(func() { close(h.closeFinish) })
	}
}

type engineFixtureBody struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *engineFixtureBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type engineFixtureStreamOptions struct {
	TimeoutMS int
	RequestID string
	HoldClose bool
}

func engineFixtureStartBlockedStream(t *testing.T, options ...engineFixtureStreamOptions) (*Engine, *generation, *engineBlockedHost, *engineFixtureBody, *engineFixtureTransport) {
	t.Helper()
	var option engineFixtureStreamOptions
	if len(options) > 0 {
		option = options[0]
	}
	settings := map[string]any{}
	if option.TimeoutMS > 0 {
		settings["timeout"] = option.TimeoutMS
	}
	host := newEngineBlockedHost()
	if option.HoldClose {
		host.closeFinish = make(chan struct{})
	}
	body := &engineFixtureBody{Reader: strings.NewReader("data: {\"id\":\"synthetic\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"), closed: make(chan struct{})}
	rt := &engineFixtureTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/console/api/v2/config":
			return engineFixtureConsole(req, "synthetic-model", settings), nil
		case "/inference/v1/chat/completions":
			return engineFixtureResponse(req, body), nil
		default:
			return nil, fmt.Errorf("unexpected synthetic destination: %s", req.URL.Path)
		}
	}}
	e := New(Dependencies{Logf: engineFixtureNoopLog, Streams: host, HTTPClientFactory: func(string) (*http.Client, error) { return &http.Client{Transport: rt}, nil }})
	t.Cleanup(func() { host.unblock(); e.Shutdown() })
	if err := e.configure(engineFixtureConfig(t, 0)); err != nil {
		t.Fatal(err)
	}
	g := engineFixtureGeneration(t, e)
	request := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "opencode/p/m", Format: "openai", SourceFormat: "openai", Payload: []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`), StorageJSON: engineFixtureJSON(t, engineFixtureCredential()), Metadata: map[string]any{"caller_scope": "engine-test", "execution_session_id": "conversation"}}, StreamID: "reused-stream-id"}
	if option.RequestID != "" {
		intercept := pluginapi.RequestInterceptRequest{RequestID: option.RequestID, Model: "opencode/p/m"}
		if _, err := e.intercept(engineFixtureJSON(t, intercept)); err != nil {
			t.Fatal(err)
		}
		request.Headers = http.Header{internalRequestHeader: {option.RequestID}}
	}
	if _, err := e.execute(engineFixtureJSON(t, request), true); err != nil {
		t.Fatal(err)
	}
	engineFixtureWait(t, host.started, "backpressured Emit")
	return e, g, host, body, rt
}

func TestEngineReconfigureClosesBackpressureAndJoinsBeforePublishing(t *testing.T) {
	t.Parallel()
	e, old, host, body, rt := engineFixtureStartBlockedStream(t)
	config := engineFixtureConfig(t, 7)
	result := make(chan error, 1)
	go func() { result <- e.configure(config) }()
	engineFixtureWait(t, host.released, "host Close during reconfigure")
	if old.root.Err() == nil {
		t.Fatal("retired generation root was not canceled")
	}
	if _, done, err := e.begin(); err == nil {
		done()
		t.Fatal("new work admitted during retirement")
	} else {
		engineFixtureStatus(t, err, 503)
	}
	e.mu.Lock()
	published := e.current
	e.mu.Unlock()
	if published != old {
		t.Fatal("new generation published before old worker joined")
	}
	select {
	case err := <-result:
		t.Fatalf("reconfigure returned before Emit exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if rt.closes.Load() != 0 {
		t.Fatal("transport closed before stream worker joined")
	}
	host.unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconfigure did not join released worker")
	}
	engineFixtureWait(t, host.exited, "Emit exit")
	engineFixtureWait(t, body.closed, "stream response body close")
	next := engineFixtureGeneration(t, e)
	if next == old || next.config.RequestTimeoutSeconds != 7 || next.root.Err() != nil {
		t.Fatal("fresh live generation not published")
	}
	if host.closes.Load() != 1 || rt.closes.Load() != 1 {
		t.Fatalf("host/transport close counts=%d/%d", host.closes.Load(), rt.closes.Load())
	}
	old.RLock()
	stale := len(old.streams) + len(old.active) + len(old.cache) + len(old.proxies)
	old.RUnlock()
	if stale != 0 {
		t.Fatal("retirement retained generation-owned state")
	}
	// A late duplicate completion must not close a reused ID in the next generation.
	next.Lock()
	next.streams["reused-stream-id"] = struct{}{}
	next.Unlock()
	e.closeStream(old, "reused-stream-id", "late old worker")
	if host.closes.Load() != 1 {
		t.Fatal("late old-generation completion closed a new stream")
	}
	next.Lock()
	delete(next.streams, "reused-stream-id")
	next.Unlock()
}

func TestEngineInvalidConfigurationPreservesActiveGeneration(t *testing.T) {
	t.Parallel()
	e := New(Dependencies{Logf: engineFixtureNoopLog})
	if err := e.configure(engineFixtureConfig(t, 0)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Shutdown)
	old, release, err := e.begin()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before := old.config
	cases := []struct {
		name string
		raw  []byte
	}{
		{"invalid-json", []byte(`{`)},
		{"old-schema", engineFixtureJSON(t, map[string]any{"schema_version": 1})},
		{"invalid-yaml", engineFixtureJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte("allowed_destinations: [")})},
		{"empty-destinations", engineFixtureJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte("allowed_destinations: []")})},
		{"invalid-timeout", engineFixtureJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte("request_timeout_seconds: -1")})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() { result <- e.configure(tc.raw) }()
			select {
			case err := <-result:
				engineFixtureStatus(t, err, 400)
			case <-time.After(time.Second):
				t.Fatal("invalid config waited for active generation")
			}
			if engineFixtureGeneration(t, e) != old || old.root.Err() != nil || !reflect.DeepEqual(before, old.config) {
				t.Fatal("invalid config changed live generation")
			}
			admitted, done, err := e.begin()
			if err != nil {
				t.Fatal(err)
			}
			done()
			if admitted != old {
				t.Fatal("invalid config swapped active generation")
			}
		})
	}
}

func TestEngineShutdownIsConcurrentIdempotentAndTerminal(t *testing.T) {
	t.Parallel()
	e, old, host, body, rt := engineFixtureStartBlockedStream(t)
	e.cooldowns.rememberAfter(cooldownKey{credential: engineFixtureCredential().fingerprint(), model: "m"}, 429, "60")
	const count = 12
	complete := make(chan struct{}, count)
	for i := 0; i < count; i++ {
		go func() { e.Shutdown(); complete <- struct{}{} }()
	}
	engineFixtureWait(t, host.released, "host Close during Shutdown")
	select {
	case <-complete:
		t.Fatal("Shutdown returned before active stream worker exited")
	case <-time.After(25 * time.Millisecond):
	}
	if _, done, err := e.begin(); err == nil {
		done()
		t.Fatal("Shutdown still admits work")
	} else {
		engineFixtureStatus(t, err, 503)
	}
	host.unblock()
	for i := 0; i < count; i++ {
		engineFixtureWait(t, complete, "concurrent Shutdown")
	}
	engineFixtureWait(t, body.closed, "shutdown response cleanup")
	engineFixtureAssertStoresEmpty(t, e)
	if old.root.Err() == nil || host.closes.Load() != 1 || rt.closes.Load() != 1 {
		t.Fatalf("shutdown not terminal/idempotent: root=%v host=%d transport=%d", old.root.Err(), host.closes.Load(), rt.closes.Load())
	}
	engineFixtureStatus(t, e.configure(engineFixtureConfig(t, 0)), 503)
	if _, done, err := e.begin(); err == nil {
		done()
		t.Fatal("terminal engine reopened")
	} else {
		engineFixtureStatus(t, err, 503)
	}
	e.Shutdown()
	if host.closes.Load() != 1 || rt.closes.Load() != 1 {
		t.Fatal("repeated Shutdown repeated external cleanup")
	}
}

func engineFixtureAssertStoresEmpty(t *testing.T, e *Engine) {
	t.Helper()
	e.sessions.mu.Lock()
	cached, linked := len(e.sessions.cache), e.sessions.lru.Len()
	e.sessions.mu.Unlock()
	e.cooldowns.Lock()
	cooldowns := len(e.cooldowns.entries)
	e.cooldowns.Unlock()
	if cached != 0 || linked != 0 || cooldowns != 0 {
		t.Fatalf("terminal engine retained identity/cooldown state: cache=%d lru=%d cooldown=%d", cached, linked, cooldowns)
	}
}

func TestEngineShutdownBeforeConfigureCannotBeReopened(t *testing.T) {
	t.Parallel()
	e := New(Dependencies{Logf: engineFixtureNoopLog})
	e.Shutdown()
	e.Shutdown()
	engineFixtureStatus(t, e.configure(engineFixtureConfig(t, 0)), 503)
	if _, done, err := e.begin(); err == nil {
		done()
		t.Fatal("shutdown-before-configure admitted work")
	} else {
		engineFixtureStatus(t, err, 503)
	}
}

func TestEngineQuiesceCanResumeOnlyWithFreshGeneration(t *testing.T) {
	t.Parallel()
	e := New(Dependencies{Logf: engineFixtureNoopLog})
	t.Cleanup(e.Shutdown)
	cfg := engineFixtureConfig(t, 0)
	if err := e.configure(cfg); err != nil {
		t.Fatal(err)
	}
	old := engineFixtureGeneration(t, e)
	c := engineFixtureCredential()
	req := pluginapi.ExecutorRequest{Metadata: map[string]any{"caller_scope": "quiesce-test", "execution_session_id": "preserved"}}
	first, err := e.sessions.headers(c, req)
	if err != nil {
		t.Fatal(err)
	}
	e.cooldowns.rememberAfter(cooldownKey{credential: c.fingerprint(), model: "m"}, 429, "60")
	e.quiesce(false)
	engineFixtureStatus(t, e.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}), 429)
	if old.root.Err() != context.Canceled {
		t.Fatal("quiesce did not cancel prior root")
	}
	if _, done, err := e.begin(); err == nil {
		done()
		t.Fatal("quiesced generation admitted work")
	} else {
		engineFixtureStatus(t, err, 503)
	}
	if err := e.configure(cfg); err != nil {
		t.Fatal(err)
	}
	if next := engineFixtureGeneration(t, e); next == old || next.root.Err() != nil {
		t.Fatal("quiesce resumed retired generation")
	}
	second, err := e.sessions.headers(c, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Get("X-Session-Id") != second.Get("X-Session-Id") {
		t.Fatal("quiesce reset stable session affinity")
	}
	engineFixtureStatus(t, e.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}), 429)
}

// Exercise the admission/Add versus retirement/Wait boundary while successive
// transitions race a terminal shutdown. Every accepted worker must join exactly
// its admitted generation; rejected work must report lifecycle unavailability.
func TestEngineConcurrentAdmissionReconfigureAndShutdown(t *testing.T) {
	t.Parallel()
	e := New(Dependencies{Logf: engineFixtureNoopLog})
	config := engineFixtureConfig(t, 0)
	if err := e.configure(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Shutdown)
	start := make(chan struct{})
	firstReconfigured := make(chan struct{})
	var firstOnce sync.Once
	var workers sync.WaitGroup
	failures := make(chan error, 32)
	report := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	isUnavailable := func(err error) bool { var status *fault.Error; return errors.As(err, &status) && status.Status == 503 }
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for j := 0; j < 100; j++ {
				g, done, err := e.begin()
				if err != nil {
					if !isUnavailable(err) {
						report(err)
					}
					runtime.Gosched()
					continue
				}
				if g == nil || done == nil {
					report(errors.New("admitted worker has no generation or release"))
					return
				}
				runtime.Gosched()
				done()
			}
		}()
	}
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for j := 0; j < 12; j++ {
				if err := e.configure(config); err != nil && !isUnavailable(err) {
					report(err)
				}
				firstOnce.Do(func() { close(firstReconfigured) })
				runtime.Gosched()
			}
		}()
	}
	workers.Add(1)
	go func() { defer workers.Done(); <-firstReconfigured; e.Shutdown() }()
	close(start)
	joined := make(chan struct{})
	go func() { workers.Wait(); close(joined) }()
	engineFixtureWait(t, joined, "concurrent admission/transition workers")
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if _, done, err := e.begin(); err == nil {
		done()
		t.Fatal("concurrent configure reopened terminal engine")
	} else {
		engineFixtureStatus(t, err, 503)
	}
	engineFixtureStatus(t, e.configure(config), 503)
}

func engineFixtureAssertCancellationJoinsHostCallback(t *testing.T, e *Engine, g *generation, host *engineBlockedHost, body *engineFixtureBody) {
	t.Helper()
	engineFixtureWait(t, host.released, "request cancellation closing backpressured host")
	// No lifecycle transition is allowed to provide the cancellation in these
	// tests: this must be the request's own timeout/completion watcher.
	if engineFixtureGeneration(t, e) != g || g.root.Err() != nil {
		t.Fatal("request cancellation retired the whole generation")
	}
	host.finishOnce.Do(func() { close(host.finish) })
	engineFixtureWait(t, host.exited, "canceled Emit returning")
	joined := make(chan struct{})
	go func() { g.workers.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("stream lease released while host Close callback was still running")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-body.closed:
		t.Fatal("stream cleanup ran before cancellation callback joined")
	default:
	}
	host.closeFinishOnce.Do(func() { close(host.closeFinish) })
	engineFixtureWait(t, host.closeExited, "host Close callback completion")
	engineFixtureWait(t, joined, "stream lease after callback join")
	engineFixtureWait(t, body.closed, "canceled stream body cleanup")
	if host.closes.Load() != 1 {
		t.Fatalf("request cancellation closed the same stream %d times", host.closes.Load())
	}
	if _, done, err := e.begin(); err != nil {
		t.Fatal("request cancellation disabled unrelated work", err)
	} else {
		done()
	}
}

func TestEngineRouteDeadlineClosesBlockedEmitAndJoinsCancellationCallback(t *testing.T) {
	t.Parallel()
	e, g, host, body, _ := engineFixtureStartBlockedStream(t, engineFixtureStreamOptions{TimeoutMS: 80, HoldClose: true})
	engineFixtureAssertCancellationJoinsHostCallback(t, e, g, host, body)
	host.mu.Lock()
	messages := strings.Join(host.messages, " ")
	host.mu.Unlock()
	if !strings.Contains(messages, "deadline") {
		t.Fatalf("deadline stream close lost timeout classification: %s", messages)
	}
}

func TestEngineRequestCompleteClosesBlockedEmitAndJoinsCancellationCallback(t *testing.T) {
	t.Parallel()
	const requestID = "complete-backpressured-request"
	e, g, host, body, _ := engineFixtureStartBlockedStream(t, engineFixtureStreamOptions{RequestID: requestID, HoldClose: true})
	if _, err := e.Handle("request.complete", engineFixtureJSON(t, pluginapi.RequestCompletion{RequestID: requestID})); err != nil {
		t.Fatal(err)
	}
	engineFixtureAssertCancellationJoinsHostCallback(t, e, g, host, body)
	g.RLock()
	active := g.active[requestID]
	g.RUnlock()
	if active != nil {
		t.Fatal("request completion retained its lifecycle record")
	}
	host.mu.Lock()
	messages := strings.Join(host.messages, " ")
	host.mu.Unlock()
	if !strings.Contains(messages, "canceled") {
		t.Fatalf("completed request close lost cancellation classification: %s", messages)
	}
}

func TestEngineLateCompletionOfRetiredRequestDoesNotCancelNewRequest(t *testing.T) {
	t.Parallel()
	e := New(Dependencies{Logf: engineFixtureNoopLog})
	t.Cleanup(e.Shutdown)
	cfg := engineFixtureConfig(t, 0)
	if err := e.configure(cfg); err != nil {
		t.Fatal(err)
	}
	intercept := func(id string) {
		t.Helper()
		_, err := e.Handle("request.intercept_before", engineFixtureJSON(t, pluginapi.RequestInterceptRequest{RequestID: id, Model: "opencode/p/m"}))
		if err != nil {
			t.Fatal(err)
		}
	}
	intercept("old-host-uuid")
	old := engineFixtureGeneration(t, e)
	oldContext, oldCancel := executionContext(old, pluginapi.ExecutorRequest{Headers: http.Header{internalRequestHeader: {"old-host-uuid"}}})
	defer oldCancel()
	if err := e.configure(cfg); err != nil {
		t.Fatal(err)
	}
	if oldContext.Err() != context.Canceled {
		t.Fatal("old request was not retired")
	}
	intercept("new-host-uuid")
	fresh := engineFixtureGeneration(t, e)
	freshContext, freshCancel := executionContext(fresh, pluginapi.ExecutorRequest{Headers: http.Header{internalRequestHeader: {"new-host-uuid"}}})
	defer freshCancel()
	for i := 0; i < 2; i++ {
		if _, err := e.Handle("request.complete", engineFixtureJSON(t, pluginapi.RequestCompletion{RequestID: "old-host-uuid"})); err != nil {
			t.Fatal(err)
		}
	}
	if freshContext.Err() != nil {
		t.Fatal("late old completion canceled an unrelated new generation request")
	}
	if _, err := e.Handle("request.complete", engineFixtureJSON(t, pluginapi.RequestCompletion{RequestID: "new-host-uuid"})); err != nil {
		t.Fatal(err)
	}
	if freshContext.Err() != context.Canceled {
		t.Fatal("new request completion did not cancel its own context")
	}
}
