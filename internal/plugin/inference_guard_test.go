package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	wire "github.com/megumin31/cpa-opencode-zen/internal/protocol"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Unique fake credentials isolate tests from credential-and-model-scoped Retry-After state
// even when the OS reuses an earlier httptest server's ephemeral port.
func guardTestCredential(t *testing.T, server string) credential {
	c := testCredential(server)
	c.Key = "fake-guard-" + strings.ReplaceAll(t.Name(), "/", "-")
	return c
}

func guardTestEndpoint(base, protocol, model string) string {
	switch protocol {
	case "openai":
		return base + "/chat/completions"
	case "openai-response":
		return base + "/responses"
	case "claude":
		return base + "/messages"
	case "gemini":
		return base + "/models/" + model + ":streamGenerateContent?alt=sse"
	default:
		panic("unknown test protocol")
	}
}

func guardTestConfig(model, endpoint string) pluginConfig {
	cfg := defaultConfig()
	cfg.TestGuard = &inferenceTestGuard{
		Enabled:         true,
		AllowedRequests: []inferenceGuardRule{{Model: model, Endpoint: endpoint}},
	}
	return cfg
}

func TestInferenceGuardConfigValidation(t *testing.T) {

	const endpoint = "https://opencode.ai/inference/v1/chat/completions"
	for _, tc := range []struct {
		name    string
		edit    func(*pluginConfig)
		wantErr bool
	}{
		{"absent_is_normal_mode", func(c *pluginConfig) { c.TestGuard = nil }, false},
		{"explicit_enabled_and_pair", func(*pluginConfig) {}, false},
		{"present_disabled", func(c *pluginConfig) { c.TestGuard.Enabled = false }, true},
		{"present_empty", func(c *pluginConfig) { c.TestGuard = &inferenceTestGuard{} }, true},
		{"nil_allowlist", func(c *pluginConfig) { c.TestGuard.AllowedRequests = nil }, true},
		{"empty_allowlist", func(c *pluginConfig) { c.TestGuard.AllowedRequests = []inferenceGuardRule{} }, true},
		{"empty_model", func(c *pluginConfig) { c.TestGuard.AllowedRequests[0].Model = "" }, true},
		{"blank_model", func(c *pluginConfig) { c.TestGuard.AllowedRequests[0].Model = " " }, true},
		{"empty_endpoint", func(c *pluginConfig) { c.TestGuard.AllowedRequests[0].Endpoint = "" }, true},
		{"relative_endpoint", func(c *pluginConfig) { c.TestGuard.AllowedRequests[0].Endpoint = "/inference/v1/chat/completions" }, true},
		{"untrusted_endpoint", func(c *pluginConfig) {
			c.TestGuard.AllowedRequests[0].Endpoint = "https://untrusted.example/inference/v1/chat/completions"
		}, true},
		{"credential_prefix_only", func(c *pluginConfig) { c.TestGuard.AllowedRequests[0].Endpoint = "https://opencode.ai/inference" }, true},
		{"endpoint_userinfo", func(c *pluginConfig) {
			c.TestGuard.AllowedRequests[0].Endpoint = "https://fake-user:fake-password@opencode.ai/inference/v1/chat/completions"
		}, true},
		{"endpoint_fragment", func(c *pluginConfig) { c.TestGuard.AllowedRequests[0].Endpoint = endpoint + "#fragment" }, true},
		{"endpoint_encoded_path", func(c *pluginConfig) {
			c.TestGuard.AllowedRequests[0].Endpoint = "https://opencode.ai/inference/%76%31/chat/completions"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := guardTestConfig("free-test-model", endpoint)
			tc.edit(&cfg)
			if err := validateInferenceGuardConfig(cfg); (err != nil) != tc.wantErr {
				t.Fatalf("validateInferenceGuardConfig error=%v, want error=%v", err, tc.wantErr)
			}
			if err := validateConfig(cfg); (err != nil) != tc.wantErr {
				t.Fatalf("validateConfig did not enforce the guard: error=%v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

func TestInferenceGuardYAMLMalformedBlocksFailClosed(t *testing.T) {
	engine := newTestEngine(t)

	const pair = "    - model: free-test-model\n      endpoint: https://opencode.ai/inference/v1/chat/completions\n"
	for _, tc := range []struct{ name, guard string }{
		{"null_block", "isolated_test_guard: null\n"},
		{"empty_block", "isolated_test_guard: {}\n"},
		{"scalar_block", "isolated_test_guard: true\n"},
		{"disabled", "isolated_test_guard:\n  enabled: false\n  allowed_requests:\n" + pair},
		{"missing_enabled", "isolated_test_guard:\n  allowed_requests:\n" + pair},
		{"missing_allowlist", "isolated_test_guard:\n  enabled: true\n"},
		{"empty_allowlist", "isolated_test_guard:\n  enabled: true\n  allowed_requests: []\n"},
		{"unknown_guard_field", "isolated_test_guard:\n  enabled: true\n  allow_paid: true\n  allowed_requests:\n" + pair},
		{"unknown_rule_field", "isolated_test_guard:\n  enabled: true\n  allowed_requests:\n" + pair + "      allow_paid: true\n"},
		{"duplicate_enabled", "isolated_test_guard:\n  enabled: true\n  enabled: false\n  allowed_requests:\n" + pair},
		{"nonstring_model", "isolated_test_guard:\n  enabled: true\n  allowed_requests:\n    - model: 123\n      endpoint: https://opencode.ai/inference/v1/chat/completions\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := engine.configure(rawJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte(tc.guard)})); err == nil {
				t.Fatal("malformed isolated guard configuration was accepted")
			}
		})
	}
	t.Run("valid_block_round_trip", func(t *testing.T) {
		cfgYAML := "isolated_test_guard:\n  enabled: true\n  allowed_requests:\n" + pair
		if err := engine.configure(rawJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte(cfgYAML)})); err != nil {
			t.Fatal(err)
		}
		cfg := engine.current.config
		if cfg.TestGuard == nil || !cfg.TestGuard.Enabled || len(cfg.TestGuard.AllowedRequests) != 1 {
			t.Fatal("valid guard configuration was not retained")
		}
		if err := engine.checkInferenceGuard(cfg, "openai", "https://opencode.ai/inference/v1/chat/completions", []byte(`{"model":"free-test-model"}`)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestInferenceGuardAbsentLeavesNormalRequestChecksUnchanged(t *testing.T) {
	engine := newTestEngine(t)

	// Guard absence must not introduce new restrictions in ordinary operation.
	// Existing request/transport validators still protect the normal executor.
	if err := engine.checkInferenceGuard(defaultConfig(), "unknown", "not-a-url", []byte("not-json")); err != nil {
		t.Fatalf("absent isolated guard changed normal-mode behavior: %v", err)
	}
}

func TestInferenceGuardConfigurationReportsActivationBeforeRequests(t *testing.T) {
	engine := newTestEngine(t)

	var output bytes.Buffer
	previous := engine.logf
	engine.logf = log.New(&output, "", 0).Printf
	defer func() { engine.logf = previous }()
	for _, tc := range []struct{ yaml, expected string }{
		{"", "configured=true enabled=false"},
		{"isolated_test_guard:\n  enabled: true\n  allowed_requests:\n    - model: private-test-model\n      endpoint: https://opencode.ai/inference/private/v1/chat/completions\n", "configured=true enabled=true"},
	} {
		output.Reset()
		if err := engine.configure(rawJSON(t, map[string]any{"schema_version": 6, "config_yaml": []byte(tc.yaml)})); err != nil {
			t.Fatal(err)
		}
		text := output.String()
		if !strings.Contains(text, tc.expected) || strings.Contains(text, "private") || strings.Contains(text, "https://") {
			t.Fatal("configuration diagnostic is missing or unsafe")
		}
	}
}

func TestInferenceGuardAcceptsFinalNativeTargets(t *testing.T) {
	engine := newTestEngine(t)

	const base = "https://opencode.ai/inference/native/v1"
	const model = "free-test-model"
	for _, protocol := range []string{"openai", "openai-response", "claude", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			engine.testConfig(t, "https://opencode.ai")
			r := catalog.Route{Alias: "opencode/p/client-alias", ProviderID: "p", ModelID: model, Package: protocolPackage(protocol), Protocol: protocol, BaseURL: base, Settings: map[string]any{}, Compatibility: map[string]any{}}
			payload, target, _, err := engine.prepareRequest(engine.current.config, context.Background(), credential{Key: "fake-isolated-test-key"}, r, wire.FormatName(protocol), []byte(clientRequest(protocol)))
			if err != nil {
				t.Fatal(err)
			}
			if target != guardTestEndpoint(base, protocol, model) {
				t.Fatal("native target unexpectedly changed")
			}
			cfg := guardTestConfig(model, target)
			if err := validateInferenceGuardConfig(cfg); err != nil {
				t.Fatalf("valid native endpoint rejected by configuration: %v", err)
			}
			if err := engine.checkInferenceGuard(cfg, protocol, target, payload); err != nil {
				t.Fatalf("final native request rejected: %v", err)
			}
		})
	}
}

func TestInferenceGuardRejectsMalformedOrAmbiguousFinalModel(t *testing.T) {
	engine := newTestEngine(t)

	for _, protocol := range []string{"openai", "openai-response", "claude"} {
		endpoint := guardTestEndpoint("https://opencode.ai/inference/v1", protocol, "free-test-model")
		cfg := guardTestConfig("free-test-model", endpoint)
		for _, tc := range []struct{ name, payload string }{
			{"missing", `{"messages":[]}`},
			{"nested_only", `{"metadata":{"model":"free-test-model"}}`},
			{"paid_override", `{"model":"paid-test-model"}`},
			{"null", `{"model":null}`},
			{"number", `{"model":123}`},
			{"boolean", `{"model":true}`},
			{"array", `{"model":["free-test-model"]}`},
			{"object", `{"model":{"id":"free-test-model"}}`},
			{"empty", `{"model":""}`},
			{"whitespace", `{"model":" "}`},
			{"padded", `{"model":" free-test-model"}`},
			{"duplicate_same", `{"model":"free-test-model","model":"free-test-model"}`},
			{"duplicate_paid_last", `{"model":"free-test-model","model":"paid-test-model"}`},
			{"duplicate_paid_first", `{"model":"paid-test-model","model":"free-test-model"}`},
			{"escaped_duplicate", `{"model":"paid-test-model","\u006dodel":"free-test-model"}`},
			{"mixed_case", `{"model":"free-test-model","Model":"paid-test-model"}`},
			{"incorrect_case", `{"Model":"free-test-model"}`},
			{"malformed", `{"model":"free-test-model"`},
			{"trailing_document", `{"model":"free-test-model"}{}`},
			{"top_level_array", `[{"model":"free-test-model"}]`},
			{"top_level_null", `null`},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				if err := engine.checkInferenceGuard(cfg, protocol, endpoint, []byte(tc.payload)); err == nil {
					t.Fatal("unapproved or ambiguous final model passed the guard")
				}
			})
		}
	}
}

func TestInferenceGuardRequiresExactModelEndpointPair(t *testing.T) {
	engine := newTestEngine(t)

	const a = "https://opencode.ai/inference/provider-a/v1/chat/completions"
	const b = "https://opencode.ai/inference/provider-b/v1/chat/completions"
	cfg := guardTestConfig("free-a", a)
	cfg.TestGuard.AllowedRequests = append(cfg.TestGuard.AllowedRequests, inferenceGuardRule{Model: "free-b", Endpoint: b})
	for _, tc := range []struct {
		name, model, endpoint string
		allowed               bool
	}{
		{"first_pair", "free-a", a, true},
		{"second_pair", "free-b", b, true},
		{"cross_product_a", "free-a", b, false},
		{"cross_product_b", "free-b", a, false},
		{"prefix_only", "free-a", "https://opencode.ai/inference/provider-a/v1", false},
		{"suffix", "free-a", a + "/extra", false},
		{"query_added", "free-a", a + "?bill=true", false},
		{"trailing_slash", "free-a", a + "/", false},
		{"other_port", "free-a", "https://opencode.ai:444/inference/provider-a/v1/chat/completions", false},
		{"untrusted_host", "free-a", "https://untrusted.example/inference/provider-a/v1/chat/completions", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := rawJSON(t, map[string]any{"model": tc.model})
			if err := engine.checkInferenceGuard(cfg, "openai", tc.endpoint, payload); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, expected=%v, error=%v", err == nil, tc.allowed, err)
			}
		})
	}
	t.Run("guard_rule_cannot_override_credential_destination", func(t *testing.T) {
		untrusted := "https://untrusted.example/inference/v1/chat/completions"
		bad := guardTestConfig("free-a", untrusted)
		if err := engine.checkInferenceGuard(bad, "openai", untrusted, []byte(`{"model":"free-a"}`)); err == nil {
			t.Fatal("guard allowlist bypassed credential destination policy")
		}
	})
	t.Run("invalid_runtime_guard_fails_closed", func(t *testing.T) {
		for _, guard := range []*inferenceTestGuard{
			{},
			{Enabled: true},
			{Enabled: false, AllowedRequests: []inferenceGuardRule{{Model: "free-a", Endpoint: a}}},
		} {
			bad := defaultConfig()
			bad.TestGuard = guard
			if err := engine.checkInferenceGuard(bad, "openai", a, []byte(`{"model":"free-a"}`)); err == nil {
				t.Fatal("invalid runtime guard was treated as ordinary mode")
			}
		}
	})
}

func TestInferenceGuardGooglePathIsAuthoritative(t *testing.T) {
	engine := newTestEngine(t)

	const base = "https://opencode.ai/inference/google/v1"
	endpoint := guardTestEndpoint(base, "gemini", "free-test-model")
	cfg := guardTestConfig("free-test-model", endpoint)
	for _, tc := range []struct {
		name, target, body string
		allowed            bool
	}{
		{"native_no_body_model", endpoint, `{"contents":[]}`, true},
		{"matching_body_model", endpoint, `{"model":"free-test-model","contents":[]}`, true},
		{"mismatched_body_model", endpoint, `{"model":"paid-test-model","contents":[]}`, false},
		{"null_body_model", endpoint, `{"model":null}`, false},
		{"numeric_body_model", endpoint, `{"model":123}`, false},
		{"duplicate_body_model", endpoint, `{"model":"paid-test-model","model":"free-test-model"}`, false},
		{"ambiguous_body_model", endpoint, `{"model":"free-test-model","Model":"paid-test-model"}`, false},
		{"malformed_body", endpoint, `{"contents":`, false},
		{"nonobject_body", endpoint, `[]`, false},
		{"different_path_model", guardTestEndpoint(base, "gemini", "paid-test-model"), `{"contents":[]}`, false},
		{"missing_sse_query", strings.TrimSuffix(endpoint, "?alt=sse"), `{"contents":[]}`, false},
		{"extra_query", endpoint + "&bill=true", `{"contents":[]}`, false},
		{"nonstream_method", strings.Replace(endpoint, ":streamGenerateContent", ":generateContent", 1), `{"contents":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := engine.checkInferenceGuard(cfg, "gemini", tc.target, []byte(tc.body)); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, expected=%v, error=%v", err == nil, tc.allowed, err)
			}
		})
	}
	t.Run("matching_endpoint_cannot_mask_different_path_model", func(t *testing.T) {
		mismatch := guardTestConfig("free-test-model", guardTestEndpoint(base, "gemini", "paid-test-model"))
		if err := engine.checkInferenceGuard(mismatch, "gemini", mismatch.TestGuard.AllowedRequests[0].Endpoint, []byte(`{"contents":[]}`)); err == nil {
			t.Fatal("allowlist model was used instead of the actual Google path model")
		}
	})
}

func TestInferenceGuardErrorRedactsRequestIdentity(t *testing.T) {
	engine := newTestEngine(t)

	const endpoint = "https://opencode.ai/inference/private-provider/v1/chat/completions"
	const model = "private-paid-model-sentinel"
	const prompt = "private-prompt-sentinel"
	cfg := guardTestConfig("free-test-model", endpoint)
	err := engine.checkInferenceGuard(cfg, "openai", endpoint, rawJSON(t, map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": prompt}}}))
	if err == nil {
		t.Fatal("unapproved final request was accepted")
	}
	for _, sensitive := range []string{model, prompt, endpoint, "private-provider", "free-test-model"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatal("guard error exposed raw request or allowlist identity")
		}
	}
}

func TestInferenceGuardLogsOnlyEnabledAndAllowedBooleans(t *testing.T) {
	engine := newTestEngine(t)

	var logs bytes.Buffer
	writer := engine.logf
	engine.logf = log.New(&logs, "", 0).Printf
	t.Cleanup(func() {
		engine.logf = writer
	})
	const endpoint = "https://opencode.ai/inference/private-provider/v1/chat/completions"
	cfg := guardTestConfig("private-free-model-sentinel", endpoint)
	for _, tc := range []struct {
		model   string
		allowed bool
	}{
		{"private-free-model-sentinel", true},
		{"private-paid-model-sentinel", false},
	} {
		logs.Reset()
		payload := rawJSON(t, map[string]any{"model": tc.model, "prompt": "private-prompt-sentinel"})
		err := engine.checkInferenceGuard(cfg, "openai", endpoint, payload)
		if (err == nil) != tc.allowed {
			t.Fatal("unexpected request decision while checking guard logging")
		}
		want := fmt.Sprintf("OpenCode isolated test guard: enabled=true allowed=%t\n", tc.allowed)
		if logs.String() != want {
			t.Fatal("guard diagnostic contains data beyond the enabled/allowed booleans")
		}
	}
	logs.Reset()
	if err := engine.checkInferenceGuard(defaultConfig(), "openai", endpoint, []byte(`{"model":"normal-model"}`)); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatal("ordinary mode emitted an isolated guard diagnostic")
	}
}

func TestInferenceGuardExecuteRejectsBeforeAnyPost(t *testing.T) {
	engine := newTestEngine(t)

	for _, protocol := range []string{"openai", "openai-response", "claude", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, reason := range []string{"paid_body_override", "null_body_model", "numeric_body_model", "wrong_endpoint", "pair_cross_product"} {
				t.Run(fmt.Sprintf("%s/stream_%t/%s", protocol, stream, reason), func(t *testing.T) {
					var posts, discoveries atomic.Int32
					var server *httptest.Server
					server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet && r.URL.Path == "/console/api/v2/config" {
							discoveries.Add(1)
							body := map[string]any{}
							switch reason {
							case "paid_body_override":
								body["model"] = "paid-test-model"
							case "null_body_model":
								body["model"] = nil
							case "numeric_body_model":
								body["model"] = 123
							}
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{"p": map[string]any{
								"package":  protocolPackage(protocol),
								"settings": map[string]any{"baseURL": server.URL + "/inference/v1"},
								"models":   map[string]any{"alias": map[string]any{"modelID": "free-test-model", "body": body}},
							}}})
							return
						}
						if r.Method == http.MethodPost {
							posts.Add(1)
						}
						// Never start an asynchronous stream if a regression sends a POST.
						w.WriteHeader(http.StatusInternalServerError)
					}))
					defer server.Close()
					engine.testConfig(t, server.URL)
					endpoint := guardTestEndpoint(server.URL+"/inference/v1", protocol, "free-test-model")
					guard := &inferenceTestGuard{Enabled: true, AllowedRequests: []inferenceGuardRule{{Model: "free-test-model", Endpoint: endpoint}}}
					if reason == "wrong_endpoint" || reason == "pair_cross_product" {
						guard.AllowedRequests[0].Endpoint = guardTestEndpoint(server.URL+"/inference/other", protocol, "free-test-model")
					}
					if reason == "pair_cross_product" {
						guard.AllowedRequests = append(guard.AllowedRequests, inferenceGuardRule{Model: "other-free-model", Endpoint: endpoint})
					}
					engine.current.Lock()
					engine.current.config.TestGuard = guard
					engine.current.Unlock()
					req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
						Model: "opencode/p/alias", Format: protocol, SourceFormat: protocol,
						Payload: []byte(clientRequest(protocol)), StorageJSON: rawJSON(t, guardTestCredential(t, server.URL)),
					}, StreamID: "isolated-guard-test-stream"}
					_, err := engine.execute(rawJSON(t, req), stream)
					if err == nil || !strings.Contains(err.Error(), "isolated test guard rejected") {
						t.Fatalf("expected final guard rejection, got %v", err)
					}
					if discoveries.Load() != 1 {
						t.Fatalf("test did not reach local config discovery: GET count=%d", discoveries.Load())
					}
					if posts.Load() != 0 {
						t.Fatalf("guard failure sent %d inference POSTs", posts.Load())
					}
				})
			}
		}
	}
}

func TestInferenceGuardExecuteAllowsMatchingFinalOverlay(t *testing.T) {
	engine := newTestEngine(t)

	for _, protocol := range []string{"openai", "openai-response", "claude", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			var posts atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/console/api/v2/config" {
					// The three body-model protocols deliberately start with a different
					// discovered model. Only the serialized overlay matches the guard.
					modelID := "discovered-model"
					body := map[string]any{"model": "free-test-model"}
					if protocol == "gemini" {
						modelID = "free-test-model"
						body = nil
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{"p": map[string]any{
						"package":  protocolPackage(protocol),
						"settings": map[string]any{"baseURL": server.URL + "/inference/v1"},
						"models":   map[string]any{"alias": map[string]any{"modelID": modelID, "body": body}},
					}}})
					return
				}
				if r.Method != http.MethodPost {
					t.Error("unexpected local mock request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				posts.Add(1)
				if server.URL+r.URL.RequestURI() != guardTestEndpoint(server.URL+"/inference/v1", protocol, "free-test-model") {
					t.Error("guarded POST used the wrong final endpoint")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error("guarded POST did not contain an object body")
				}
				if protocol == "gemini" {
					if _, present := body["model"]; present {
						t.Error("native Google model unexpectedly moved to the body")
					}
				} else if body["model"] != "free-test-model" {
					t.Error("guarded POST did not contain the allowed final overlay model")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, nativeFixture(protocol))
			}))
			defer server.Close()
			engine.testConfig(t, server.URL)
			engine.current.Lock()
			engine.current.config.TestGuard = &inferenceTestGuard{Enabled: true, AllowedRequests: []inferenceGuardRule{{
				Model: "free-test-model", Endpoint: guardTestEndpoint(server.URL+"/inference/v1", protocol, "free-test-model"),
			}}}
			engine.current.Unlock()
			req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "opencode/p/alias", Format: protocol, SourceFormat: protocol, Payload: []byte(clientRequest(protocol)), StorageJSON: rawJSON(t, guardTestCredential(t, server.URL))}}
			if _, err := engine.execute(rawJSON(t, req), false); err != nil {
				t.Fatal(err)
			}
			if posts.Load() != 1 {
				t.Fatalf("valid guarded request POST count=%d, want 1", posts.Load())
			}
		})
	}
}

func TestInferenceGuardAbsentPreservesConfiguredModelOverlay(t *testing.T) {
	engine := newTestEngine(t)

	var posts atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/console/api/v2/config" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"providers":{"p":{"package":"@opencode/ai/providers/openai/chat","settings":{"baseURL":%q},"models":{"m":{"body":{"model":"configured-native-model"}}}}}}`, server.URL+"/inference/v1")
			return
		}
		if r.Method != http.MethodPost {
			t.Error("unexpected local mock request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		posts.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "configured-native-model" {
			t.Error("normal mode no longer preserves the final configured model")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nativeFixture("openai"))
	}))
	defer server.Close()
	engine.testConfig(t, server.URL)
	if engine.current.config.TestGuard != nil {
		t.Fatal("normal configuration unexpectedly enables the isolated guard")
	}
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "opencode/p/m", Format: "openai", SourceFormat: "openai", Payload: []byte(clientRequest("openai")), StorageJSON: rawJSON(t, guardTestCredential(t, server.URL))}}
	if _, err := engine.execute(rawJSON(t, req), false); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("normal request POST count=%d, want 1", posts.Load())
	}
}
