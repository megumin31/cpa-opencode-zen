package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type engineFixtureClock struct{ nanos atomic.Int64 }

func newEngineFixtureClock(at time.Time) *engineFixtureClock {
	c := &engineFixtureClock{}
	c.nanos.Store(at.UnixNano())
	return c
}
func (c *engineFixtureClock) now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *engineFixtureClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

type engineFixtureLog struct {
	mu       sync.Mutex
	messages []string
}

func (l *engineFixtureLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}
func (l *engineFixtureLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.messages, "\n")
}
func (l *engineFixtureLog) reset() { l.mu.Lock(); defer l.mu.Unlock(); l.messages = nil }

func engineFixtureDiscover(t *testing.T, e *Engine, c credential) (*generation, map[string]catalog.Route) {
	t.Helper()
	g, done, err := e.begin()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	routes, err := e.discover(g, context.Background(), c, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return g, routes
}

func TestEngineInstancesIsolateClockLogsHTTPAndMutableCaches(t *testing.T) {
	t.Parallel()
	firstClock := newEngineFixtureClock(time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC))
	secondClock := newEngineFixtureClock(firstClock.now().Add(time.Hour))
	var firstLog, secondLog engineFixtureLog
	var firstFactories, secondFactories atomic.Int64
	makeRT := func(model string) *engineFixtureTransport {
		return &engineFixtureTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "engine-test.invalid" {
				return nil, fmt.Errorf("unexpected host in isolated fixture")
			}
			if req.URL.Path == "/console/api/v2/config" {
				return engineFixtureConsole(req, model), nil
			}
			return engineFixtureResponse(req, io.NopCloser(strings.NewReader(model))), nil
		}}
	}
	firstHTTP, secondHTTP := makeRT("model-first"), makeRT("model-second")
	first := New(Dependencies{Now: firstClock.now, Logf: firstLog.printf, Random: strings.NewReader(strings.Repeat("A", 128)), HTTPClientFactory: func(string) (*http.Client, error) {
		firstFactories.Add(1)
		return &http.Client{Transport: firstHTTP}, nil
	}})
	second := New(Dependencies{Now: secondClock.now, Logf: secondLog.printf, Random: strings.NewReader(strings.Repeat("B", 128)), HTTPClientFactory: func(string) (*http.Client, error) {
		secondFactories.Add(1)
		return &http.Client{Transport: secondHTTP}, nil
	}})
	t.Cleanup(first.Shutdown)
	t.Cleanup(second.Shutdown)
	if err := first.configure(engineFixtureConfig(t, 0)); err != nil {
		t.Fatal(err)
	}
	if err := second.configure(engineFixtureConfig(t, 0)); err != nil {
		t.Fatal(err)
	}
	firstLog.reset()
	secondLog.reset()
	c := engineFixtureCredential()
	firstGeneration, firstRoutes := engineFixtureDiscover(t, first, c)
	if firstRoutes["opencode/p/m"].ModelID != "model-first" {
		t.Fatal("first engine used another HTTP dependency")
	}
	if firstFactories.Load() != 1 || firstHTTP.requests.Load() != 1 || secondFactories.Load() != 0 || secondHTTP.requests.Load() != 0 {
		t.Fatal("HTTP factory or request escaped its engine")
	}
	if !strings.Contains(firstLog.text(), "stage=console_config") || secondLog.text() != "" {
		t.Fatal("discovery log escaped its injected sink")
	}
	firstLogBefore := firstLog.text()
	secondGeneration, secondRoutes := engineFixtureDiscover(t, second, c)
	if secondRoutes["opencode/p/m"].ModelID != "model-second" || firstLog.text() != firstLogBefore || !strings.Contains(secondLog.text(), "stage=console_config") {
		t.Fatal("second engine reused first catalog or log sink")
	}
	if firstFactories.Load() != 1 || secondFactories.Load() != 1 {
		t.Fatal("engines shared HTTP pool despite independent factories")
	}
	key := c.fingerprint() + "\x00"
	firstGeneration.RLock()
	firstExpiry := firstGeneration.cache[key].expires
	firstGeneration.RUnlock()
	secondGeneration.RLock()
	secondExpiry := secondGeneration.cache[key].expires
	secondGeneration.RUnlock()
	if !firstExpiry.Equal(firstClock.now().Add(time.Minute)) || !secondExpiry.Equal(secondClock.now().Add(time.Minute)) {
		t.Fatal("catalog expiry used another engine's clock")
	}
	if !first.authData(c, "", "").NextRefreshAfter.Equal(firstClock.now().Add(time.Minute)) || !second.authData(c, "", "").NextRefreshAfter.Equal(secondClock.now().Add(time.Minute)) {
		t.Fatal("auth refresh time ignored injected clock")
	}
	request := pluginapi.ExecutorRequest{Metadata: map[string]any{"caller_scope": "shared-caller", "execution_session_id": "same-conversation"}}
	firstSession, err := first.sessions.headers(c, request)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := second.sessions.headers(c, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.sessions == second.sessions || firstSession.Get("X-Session-Id") == secondSession.Get("X-Session-Id") {
		t.Fatal("session cache/counter/clock was shared between engines")
	}
	// Verify both independent entropy providers without relying on global rand.
	firstRandom, err := first.sessions.format("random-first", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondRandom, err := second.sessions.format("random-second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstRandom[len(firstRandom)-14:] == secondRandom[len(secondRandom)-14:] {
		t.Fatal("independent random readers were ignored")
	}
	first.cooldowns.rememberAfter(cooldownKey{credential: c.fingerprint(), model: "m"}, 429, "10")
	engineFixtureStatus(t, first.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}), 429)
	if err := second.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}); err != nil {
		t.Fatal("cooldown leaked to second engine", err)
	}
	second.cooldowns.rememberAfter(cooldownKey{credential: c.fingerprint(), model: "m"}, 503, "10")
	firstClock.advance(11 * time.Second)
	if err := first.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}); err != nil {
		t.Fatal("first clock advance did not expire its cooldown", err)
	}
	engineFixtureStatus(t, second.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}), 503)
	// Reconfiguration and shutdown of one instance cannot cancel the other root
	// or close its injected transport, even for identical endpoint and proxy keys.
	secondRoot := secondGeneration.root
	first.Shutdown()
	if firstHTTP.closes.Load() != 1 || secondHTTP.closes.Load() != 0 || secondRoot.Err() != nil {
		t.Fatal("first shutdown retired resources belonging to second engine")
	}
	admitted, done, err := second.begin()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := outbound(admitted, context.Background(), c, "", "GET", engineFixtureOrigin+"/still-live", nil, nil, true)
	done()
	if err != nil {
		t.Fatal("second engine became unusable after first shutdown", err)
	}
	payload, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(payload) != "model-second" {
		t.Fatal("wrong surviving HTTP client", string(payload), err)
	}
}

func TestEngineReconfigurePreservesAffinityAndCooldownButReplacesGenerationState(t *testing.T) {
	t.Parallel()
	clock := newEngineFixtureClock(time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC))
	var factoryCalls, consoleCalls atomic.Int64
	var clientsMu sync.Mutex
	var clients []*engineFixtureTransport
	e := New(Dependencies{Now: clock.now, Logf: engineFixtureNoopLog, HTTPClientFactory: func(string) (*http.Client, error) {
		factoryCalls.Add(1)
		rt := &engineFixtureTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/console/api/v2/config" {
				return nil, fmt.Errorf("unexpected synthetic path")
			}
			n := consoleCalls.Add(1)
			return engineFixtureConsole(req, fmt.Sprintf("generation-model-%d", n)), nil
		}}
		clientsMu.Lock()
		clients = append(clients, rt)
		clientsMu.Unlock()
		return &http.Client{Transport: rt}, nil
	}})
	t.Cleanup(e.Shutdown)
	if err := e.configure(engineFixtureConfig(t, 3)); err != nil {
		t.Fatal(err)
	}
	c := engineFixtureCredential()
	request := pluginapi.ExecutorRequest{Metadata: map[string]any{"caller_scope": "stable-caller", "execution_session_id": "stable-conversation"}}
	firstSession, err := e.sessions.headers(c, request)
	if err != nil {
		t.Fatal(err)
	}
	sessions := e.sessions
	e.cooldowns.rememberAfter(cooldownKey{credential: c.fingerprint(), model: "m"}, 429, "60")
	old, routes := engineFixtureDiscover(t, e, c)
	if routes["opencode/p/m"].ModelID != "generation-model-1" {
		t.Fatal("initial route missing")
	}
	rememberHostProxy(old, c, "http://synthetic-proxy.invalid:8080")
	intercept := pluginapi.RequestInterceptRequest{RequestID: "old-active-request", Model: "opencode/p/m"}
	if _, err := e.intercept(engineFixtureJSON(t, intercept)); err != nil {
		t.Fatal(err)
	}
	old.RLock()
	active := old.active[intercept.RequestID]
	old.RUnlock()
	if active == nil {
		t.Fatal("active request fixture was not registered")
	}
	oldClient, err := old.transport.Client("")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.configure(engineFixtureConfig(t, 9)); err != nil {
		t.Fatal(err)
	}
	next := engineFixtureGeneration(t, e)
	if old == next || old.transport == next.transport || old.root.Err() == nil || next.root.Err() != nil {
		t.Fatal("reconfigure reused retired generation or HTTP pool")
	}
	if old.config.RequestTimeoutSeconds != 3 || next.config.RequestTimeoutSeconds != 9 {
		t.Fatal("configuration snapshot was mutated in place")
	}
	engineFixtureWait(t, active.ctx.Done(), "retired active request cancellation")
	old.RLock()
	oldCount := len(old.active) + len(old.cache) + len(old.proxies) + len(old.streams)
	old.RUnlock()
	next.RLock()
	nextCount := len(next.active) + len(next.cache) + len(next.proxies) + len(next.streams)
	next.RUnlock()
	if oldCount != 0 || nextCount != 0 || storedHostProxy(next, c) != "" {
		t.Fatal("generation-owned routes/proxies/requests/streams survived replacement")
	}
	if _, err := old.transport.Client(""); err == nil {
		t.Fatal("retired HTTP pool could be repopulated")
	} else {
		engineFixtureStatus(t, err, 503)
	}
	nextClient, err := next.transport.Client("")
	if err != nil {
		t.Fatal(err)
	}
	if nextClient == oldClient || factoryCalls.Load() != 2 {
		t.Fatal("new generation reused retired HTTP client")
	}
	clientsMu.Lock()
	firstClose, secondClose := clients[0].closes.Load(), clients[1].closes.Load()
	clientsMu.Unlock()
	if firstClose != 1 || secondClose != 0 {
		t.Fatalf("wrong generation transport cleanup: %d/%d", firstClose, secondClose)
	}
	secondSession, err := e.sessions.headers(c, request)
	if err != nil {
		t.Fatal(err)
	}
	if e.sessions != sessions || firstSession.Get("X-Session-Id") != secondSession.Get("X-Session-Id") {
		t.Fatal("ordinary reconfiguration reset established session affinity")
	}
	engineFixtureStatus(t, e.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}), 429)
	_, freshRoutes := engineFixtureDiscover(t, e, c)
	if freshRoutes["opencode/p/m"].ModelID != "generation-model-2" || consoleCalls.Load() != 2 {
		t.Fatal("fresh generation served stale route cache")
	}
	clock.advance(61 * time.Second)
	if err := e.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}); err != nil {
		t.Fatal("retained cooldown no longer follows injected clock", err)
	}
}

func TestEngineStreamsWithSameExternalIDStayInstanceScoped(t *testing.T) {
	t.Parallel()
	first, _, firstHost, _, _ := engineFixtureStartBlockedStream(t)
	second, _, secondHost, _, _ := engineFixtureStartBlockedStream(t)
	firstStopped := make(chan struct{})
	go func() { first.Shutdown(); close(firstStopped) }()
	engineFixtureWait(t, firstHost.released, "first instance stream close")
	select {
	case <-secondHost.released:
		t.Fatal("first shutdown closed the second instance stream")
	case <-time.After(25 * time.Millisecond):
	}
	if _, done, err := second.begin(); err != nil {
		t.Fatal("first shutdown stopped second admission", err)
	} else {
		done()
	}
	firstHost.unblock()
	engineFixtureWait(t, firstStopped, "first instance shutdown")
	if firstHost.closes.Load() != 1 || secondHost.closes.Load() != 0 {
		t.Fatal("same stream ID collided across engine instances")
	}
	secondStopped := make(chan struct{})
	go func() { second.Shutdown(); close(secondStopped) }()
	engineFixtureWait(t, secondHost.released, "second instance stream close")
	secondHost.unblock()
	engineFixtureWait(t, secondStopped, "second instance shutdown")
	if firstHost.closes.Load() != 1 || secondHost.closes.Load() != 1 {
		t.Fatal("instance shutdown repeated another stream close")
	}
}

func TestEngineSessionLRUEvictsOnlyLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprintf("capacity-%d", capacity), func(t *testing.T) {
			clock := newEngineFixtureClock(time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC))
			e := New(Dependencies{Now: clock.now, Logf: engineFixtureNoopLog})
			t.Cleanup(e.Shutdown)
			e.sessions.capacity = capacity
			seed := []byte("abcdefghijklmno")
			get := func(key string) string {
				t.Helper()
				id, err := e.sessions.format(key, seed)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			expect := func(keys ...string) {
				t.Helper()
				e.sessions.mu.Lock()
				defer e.sessions.mu.Unlock()
				if len(e.sessions.cache) != len(keys) || e.sessions.lru.Len() != len(keys) {
					t.Fatalf("LRU reset or overflow: cache=%d list=%d want=%d", len(e.sessions.cache), e.sessions.lru.Len(), len(keys))
				}
				for _, key := range keys {
					if e.sessions.cache[key] == nil {
						t.Fatalf("expected cached scope %q", key)
					}
				}
			}
			first := get("first")
			second := get("second")
			if capacity == 1 {
				expect("second")
				if get("second") != second {
					t.Fatal("current cache entry changed on hit")
				}
				if get("first") == first {
					t.Fatal("least-recent entry was not actually evicted")
				}
				expect("first")
			} else {
				expect("first", "second")
				if get("first") != first {
					t.Fatal("cache hit changed stable identity")
				}
				get("third")
				expect("first", "third")
				if get("first") != first {
					t.Fatal("inserting a third scope cleared recently used identity")
				}
				if get("second") == second {
					t.Fatal("least-recent identity was not evicted")
				}
				expect("first", "second")
			}
			e.Shutdown()
			engineFixtureAssertStoresEmpty(t, e)
		})
	}
}

type engineFixtureFailingRandom struct{}

func (engineFixtureFailingRandom) Read([]byte) (int, error) {
	return 0, errors.New("synthetic-private-reader-error-do-not-echo")
}

func TestEngineRandomFailuresReturnGenericErrorsWithoutLeakingReaderDetail(t *testing.T) {
	t.Parallel()
	e := New(Dependencies{Random: engineFixtureFailingRandom{}, Logf: engineFixtureNoopLog})
	t.Cleanup(e.Shutdown)
	if headers, err := e.sessions.headers(engineFixtureCredential(), pluginapi.ExecutorRequest{}); err == nil || headers != nil {
		t.Fatal("unscoped affinity ignored entropy failure", headers, err)
	} else {
		engineFixtureStatus(t, err, 500)
		if err.Error() != "OpenCode plugin: cannot create request affinity" {
			t.Fatalf("entropy failure exposed private reader detail: %v", err)
		}
	}
	if id, err := e.sessions.format("failed-scope", nil); err == nil || id != "" {
		t.Fatal("session creation ignored entropy failure", id, err)
	} else {
		engineFixtureStatus(t, err, 500)
		if err.Error() != "OpenCode plugin: cannot create request affinity" {
			t.Fatalf("entropy failure exposed private reader detail: %v", err)
		}
	}
	e.sessions.mu.Lock()
	cached, linked := len(e.sessions.cache), e.sessions.lru.Len()
	e.sessions.mu.Unlock()
	if cached != 0 || linked != 0 {
		t.Fatal("failed entropy request installed a partial session")
	}
	// Explicit caller scope derives a sufficient HMAC seed and does not require
	// entropy; a failed unrelated request must not poison that supported path.
	request := pluginapi.ExecutorRequest{Metadata: map[string]any{"caller_scope": "trusted-caller", "execution_session_id": "explicit-session"}}
	if headers, err := e.sessions.headers(engineFixtureCredential(), request); err != nil || headers.Get("X-Session-Id") == "" {
		t.Fatal("failed entropy request poisoned explicitly scoped session", headers, err)
	}
	e.Shutdown()
	engineFixtureAssertStoresEmpty(t, e)
}
