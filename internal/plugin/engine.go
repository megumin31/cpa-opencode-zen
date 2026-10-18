package plugin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/megumin31/cpa-opencode-zen/internal/transport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// StreamHost is the only host capability used by the runtime. Implementations
// must allow Close to release a blocked Emit during quiescence.
type StreamHost interface {
	Emit(id string, payload []byte) error
	Close(id, message string) error
}

// Dependencies are fixed at construction. Nil fields select production defaults.
// An Engine owns all mutable caches; only the read-only SDK registry is shared.
type Dependencies struct {
	Now               func() time.Time
	Random            io.Reader
	Logf              func(string, ...any)
	HTTPClientFactory func(string) (*http.Client, error)
	Streams           StreamHost
}

// Engine owns one plugin instance and its serialized configuration lifecycle.
// Construct it with New; the zero value is not ready to serve requests.
type Engine struct {
	transition    sync.Mutex
	mu            sync.Mutex
	current       *generation
	accepting     bool
	closed        bool
	now           func() time.Time
	logf          func(string, ...any)
	clientFactory func(string) (*http.Client, error)
	streams       StreamHost
	sessions      *sessionManager
	cooldowns     cooldownStore
}

type generation struct {
	sync.RWMutex
	config     pluginConfig // immutable after publication; request-local snapshot
	cache      map[string]*discoveryEntry
	active     map[string]*activeRequest
	proxies    map[string]string
	streams    map[string]struct{}
	closing    bool
	root       context.Context
	rootCancel context.CancelFunc
	workers    sync.WaitGroup
	transport  *transport.Pool
}

type activeRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type pluginConfig struct {
	WorkerEndpoint        string              `yaml:"worker_endpoint"`
	WorkerToken           string              `yaml:"worker_token"`
	AllowedDestinations   []string            `yaml:"allowed_destinations"`
	AllowLoopbackHTTP     bool                `yaml:"allow_loopback_http"`
	CatalogFile           string              `yaml:"catalog_file"`
	ProxyURL              string              `yaml:"proxy_url"`
	RequestTimeoutSeconds int                 `yaml:"request_timeout_seconds"`
	TestGuard             *inferenceTestGuard `yaml:"isolated_test_guard"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{AllowedDestinations: []string{"https://opencode.ai/console", "https://opencode.ai/inference"}}
}
func (c pluginConfig) transportPolicy() transport.Policy {
	return transport.Policy{AllowedDestinations: c.AllowedDestinations, AllowLoopbackHTTP: c.AllowLoopbackHTTP, ProxyURL: c.ProxyURL, WorkerEndpoint: c.WorkerEndpoint, WorkerToken: c.WorkerToken}
}

func New(deps Dependencies) *Engine {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Random == nil {
		deps.Random = rand.Reader
	}
	if deps.Logf == nil {
		deps.Logf = log.New(os.Stderr, "", log.LstdFlags).Printf
	}
	e := &Engine{now: deps.Now, logf: deps.Logf, clientFactory: deps.HTTPClientFactory, streams: deps.Streams}
	e.sessions = newSessionManager(func() time.Time { return e.now() }, deps.Random)
	e.cooldowns = cooldownStore{entries: make(map[cooldownKey]cooldown), now: func() time.Time { return e.now() }}
	return e
}

func (e *Engine) begin() (*generation, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.accepting || e.closed || e.current == nil {
		return nil, nil, fail(503, "plugin is not accepting requests")
	}
	g := e.current
	// Admission and the transition to non-accepting are serialized by e.mu.
	// No Add can race a zero-count Wait after retirement starts.
	g.workers.Add(1)
	return g, g.workers.Done, nil
}

func (e *Engine) configure(raw []byte) error {
	var req struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return fail(400, "invalid registration request")
	}
	if req.SchemaVersion < 2 {
		return fail(400, "CPA schema version 2 or newer required")
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if err := validateInferenceGuardYAML(req.ConfigYAML); err != nil {
			return err
		}
		if yaml.Unmarshal(req.ConfigYAML, &cfg) != nil {
			return fail(400, "invalid plugin YAML")
		}
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	// Validation never modifies the live generation. Lifecycle changes are
	// serialized separately from request/map locks and all blocking I/O.
	e.transition.Lock()
	defer e.transition.Unlock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fail(503, "plugin has shut down")
	}
	e.accepting = false
	previous := e.current
	e.mu.Unlock()
	e.retire(previous, "plugin reconfigured")
	root, cancel := context.WithCancel(context.Background())
	g := &generation{config: cfg, cache: map[string]*discoveryEntry{}, active: map[string]*activeRequest{}, proxies: map[string]string{}, streams: map[string]struct{}{}, root: root, rootCancel: cancel, transport: transport.NewPool(func(proxy string) (*http.Client, error) {
		if e.clientFactory != nil {
			return e.clientFactory(proxy)
		}
		return transport.NewClient(proxy)
	})}
	e.mu.Lock()
	e.current = g
	e.accepting = true
	e.mu.Unlock()
	e.logf("OpenCode isolated test guard: configured=true enabled=%t strict_text_only=%t", cfg.TestGuard != nil, cfg.TestGuard != nil && cfg.TestGuard.StrictTextOnly)
	return nil
}

func (e *Engine) retire(g *generation, message string) {
	if g == nil {
		return
	}
	g.Lock()
	g.closing = true
	active := g.active
	g.active = map[string]*activeRequest{}
	ids := make([]string, 0, len(g.streams))
	for id := range g.streams {
		ids = append(ids, id)
	}
	g.Unlock()
	g.rootCancel()
	for _, a := range active {
		a.cancel()
	}
	// Host Emit has an independent C callback context. Cancellation alone cannot
	// release a full bridge; close it while callbacks are still valid, then join.
	for _, id := range ids {
		e.closeStream(g, id, message)
	}
	g.workers.Wait()
	g.transport.Close()
	g.Lock()
	g.cache = map[string]*discoveryEntry{}
	g.proxies = map[string]string{}
	g.Unlock()
}

func (e *Engine) quiesce(final bool) {
	e.transition.Lock()
	defer e.transition.Unlock()
	e.mu.Lock()
	e.accepting = false
	g := e.current
	if final {
		e.closed = true
	}
	e.mu.Unlock()
	e.retire(g, "plugin shutting down")
	if final {
		// No admitted worker can access these stores after retirement joins. They
		// survive configuration generations, but not the terminal Engine lifetime.
		e.sessions.mu.Lock()
		clear(e.sessions.cache)
		e.sessions.lru.Init()
		e.sessions.mu.Unlock()
		e.cooldowns.Lock()
		clear(e.cooldowns.entries)
		e.cooldowns.Unlock()
	}
}

// Shutdown is terminal and idempotent. The ABI must keep host pointers alive
// until it returns. Quiesce permits a later validated reconfigure for rollback.
func (e *Engine) Shutdown() { e.quiesce(true) }

func executionContext(g *generation, req pluginapi.ExecutorRequest) (context.Context, context.CancelFunc) {
	timeout := time.Duration(g.config.RequestTimeoutSeconds) * time.Second
	g.RLock()
	active := g.active[req.Headers.Get(internalRequestHeader)]
	root := g.root
	g.RUnlock()
	if active != nil {
		root = active.ctx
	}
	if timeout > 0 {
		return context.WithTimeout(root, timeout)
	}
	return context.WithCancel(root)
}
func discoveryContext(g *generation) (context.Context, context.CancelFunc) {
	return context.WithTimeout(g.root, 30*time.Second)
}
func rememberHostProxy(g *generation, c credential, proxy string) {
	g.Lock()
	defer g.Unlock()
	g.proxies[c.fingerprint()] = proxy
}
func storedHostProxy(g *generation, c credential) string {
	g.RLock()
	defer g.RUnlock()
	return g.proxies[c.fingerprint()]
}

func validateConfig(cfg pluginConfig) error {
	if cfg.RequestTimeoutSeconds < 0 || cfg.RequestTimeoutSeconds > 3600 {
		return fail(400, "request_timeout_seconds must be between 0 (disabled) and 3600")
	}
	if err := transport.ValidatePolicy(cfg.transportPolicy()); err != nil {
		return err
	}
	return validateInferenceGuardConfig(cfg)
}
