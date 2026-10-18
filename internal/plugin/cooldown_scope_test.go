package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRequestCooldownKeyUsesFinalUpstreamModel(t *testing.T) {
	c := engineFixtureCredential()
	want := cooldownKey{credential: c.fingerprint(), model: "upstream-model"}
	for _, tc := range []struct {
		name, protocol, routeModel, alias, body string
	}{
		{"resolved_alias", "openai", "upstream-model", "opencode/p/friendly", `{"model":"upstream-model"}`},
		{"reasoning_alias", "openai", "upstream-model", "opencode/p/friendly(high)", `{"model":"upstream-model","reasoning_effort":"high"}`},
		{"explicit_variant", "openai", "upstream-model", "opencode/p/friendly/variant/high", `{"model":"upstream-model","reasoning_effort":"high"}`},
		{"body_overlay", "openai", "configured-model", "opencode/p/other", `{"model":"upstream-model"}`},
		{"responses", "openai-response", "upstream-model", "", `{"model":"upstream-model"}`},
		{"claude", "claude", "upstream-model", "", `{"model":"upstream-model"}`},
		{"gemini_url", "gemini", "upstream-model", "", `{}`},
		{"gemini_prefix", "gemini", "models/upstream-model", "", `{}`},
		{"gemini_ignores_body_model", "gemini", "models/upstream-model", "", `{"model":"not-the-URL-model"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestCooldownKey(c, catalog.Route{Protocol: tc.protocol, ModelID: tc.routeModel, Alias: tc.alias}, []byte(tc.body))
			if err != nil || got != want {
				t.Fatalf("key=%v error=%v, want %v", got, err, want)
			}
		})
	}
	for _, model := range []string{"Upstream-model", "upstream-model(high)", "models/upstream-model"} {
		payload, _ := json.Marshal(map[string]string{"model": model})
		got, err := requestCooldownKey(c, catalog.Route{Protocol: "openai"}, payload)
		if err != nil || got == want || got.model != model {
			t.Fatalf("guessed equivalence of actual wire model %q: %v, %v", model, got, err)
		}
	}
	for _, body := range []string{`{}`, `{"model":null}`, `{"model":123}`, `{"model":{}}`, `{"model":""}`, `invalid`} {
		_, err := requestCooldownKey(c, catalog.Route{Protocol: "openai", ModelID: "fallback-must-not-hide-invalid-wire-model"}, []byte(body))
		engineFixtureStatus(t, err, 400)
	}
}

func TestCooldownSeparatesModelAndCredentialDimensions(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newTestEngine(t)
			c := engineFixtureCredential()
			key := cooldownKey{credential: c.fingerprint(), model: "limited-model"}
			e.cooldowns.rememberAfter(key, status, "27012")
			engineFixtureStatus(t, e.cooldowns.check(key), status)
			if err := e.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "other-model"}); err != nil {
				t.Fatal("model A restricted model B", err)
			}
			for _, change := range []func(*credential){
				func(c *credential) { c.Key += "-other" },
				func(c *credential) { c.Server += "/other" },
				func(c *credential) { c.OrgID += "-other" },
				func(c *credential) { c.ProxyURL = "http://other.invalid:8080" },
			} {
				other := c
				change(&other)
				if err := e.cooldowns.check(cooldownKey{credential: other.fingerprint(), model: key.model}); err != nil {
					t.Fatal("restriction crossed credential fingerprint", err)
				}
			}
		})
	}
}

func TestCooldownHeadersKeepExactDeadlineWithoutLocalExtension(t *testing.T) {
	start := time.Date(2026, 10, 18, 16, 29, 48, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		headers http.Header
		delay   time.Duration
	}{
		{"seconds_long", http.Header{"Retry-After": {"27012"}}, 27012 * time.Second},
		{"milliseconds", http.Header{"Retry-After-Ms": {"1500"}, "Retry-After": {"99"}}, 1500 * time.Millisecond},
		{"fractional_milliseconds", http.Header{"Retry-After-Ms": {"1.5"}}, 1500 * time.Microsecond},
		{"HTTP_date", http.Header{"Retry-After": {start.Add(time.Hour).Format(http.TimeFormat)}}, time.Hour},
		{"invalid_ms_fallback", http.Header{"Retry-After-Ms": {"NaN"}, "Retry-After": {"2"}}, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newEngineFixtureClock(start)
			s := cooldownStore{entries: make(map[cooldownKey]cooldown), now: clock.now}
			key := cooldownKey{credential: "synthetic-fingerprint", model: "limited-model"}
			s.rememberHeaders(key, 429, tc.headers)
			want := start.Add(tc.delay)
			if got := s.entries[key].until; !got.Equal(want) {
				t.Fatalf("deadline=%s, want %s", got, want)
			}
			clock.advance(tc.delay - time.Nanosecond)
			for range 3 {
				engineFixtureStatus(t, s.check(key), 429)
				if got := s.entries[key].until; !got.Equal(want) {
					t.Fatal("local check extended the upstream deadline", got, want)
				}
			}
			clock.advance(time.Nanosecond)
			if err := s.check(key); err != nil || len(s.entries) != 0 {
				t.Fatalf("did not expire exactly at deadline: %v, entries=%d", err, len(s.entries))
			}
		})
	}
}

func TestCooldownOnlyUpstreamRestrictionCanExtendDeadline(t *testing.T) {
	clock := newEngineFixtureClock(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	s := cooldownStore{entries: make(map[cooldownKey]cooldown), now: clock.now}
	key := cooldownKey{credential: "synthetic-fingerprint", model: "limited-model"}
	s.rememberAfter(key, 429, "60")
	original := s.entries[key]
	for _, value := range []string{"", "invalid", "-1", "0", "10", clock.now().Add(-time.Hour).Format(http.TimeFormat)} {
		s.rememberAfter(key, 503, value)
		if s.entries[key] != original {
			t.Fatal("shorter or invalid response erased the longer restriction", value)
		}
	}
	s.rememberHeaders(key, 500, http.Header{"Retry-After": {"120"}})
	if s.entries[key] != original {
		t.Fatal("unsupported HTTP status created a restriction")
	}
	s.rememberAfter(key, 503, "120")
	if !s.entries[key].until.Equal(clock.now().Add(120*time.Second)) || s.entries[key].status != 503 {
		t.Fatal("new longer upstream restriction was lost")
	}
}

func TestExecuteCooldownModelScopeSurvivesReload(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			clock := newEngineFixtureClock(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
			var limitedCalls, otherCalls atomic.Int64
			rt := &engineFixtureTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/api/v2/config") {
					return engineFixtureResponse(req, io.NopCloser(strings.NewReader(`{"providers":{"p":{"package":"@opencode/ai/providers/openai/chat","settings":{"baseURL":"`+engineFixtureOrigin+`/inference/v1"},"models":{"a":{"modelID":"limited-model","variants":[{"id":"high","body":{"reasoning_effort":"high"}}]},"alias":{"modelID":"limited-model"},"a(high)":{"modelID":"limited-model","body":{"reasoning_effort":"high"}},"overlay":{"modelID":"other-model","body":{"model":"limited-model"}},"b":{"modelID":"other-model"}}}}}`))), nil
				}
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model == "limited-model" {
					limitedCalls.Add(1)
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":"synthetic limit"}`)), Request: req}, nil
				}
				if body.Model != "other-model" {
					t.Errorf("unexpected wire model %q", body.Model)
				}
				otherCalls.Add(1)
				return engineFixtureResponse(req, io.NopCloser(strings.NewReader(nativeFixture("openai")))), nil
			}}
			e := New(Dependencies{Now: clock.now, Logf: engineFixtureNoopLog, HTTPClientFactory: func(string) (*http.Client, error) { return &http.Client{Transport: rt}, nil }})
			t.Cleanup(e.Shutdown)
			cfg := engineFixtureConfig(t, 0)
			if err := e.configure(cfg); err != nil {
				t.Fatal(err)
			}
			c := engineFixtureCredential()
			execute := func(model string, credential credential) error {
				req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "opencode/p/" + model, Format: "openai", SourceFormat: "openai", Payload: []byte(clientRequest("openai")), StorageJSON: rawJSON(t, credential)}}
				_, err := e.execute(rawJSON(t, req), false)
				return err
			}
			engineFixtureStatus(t, execute("a", c), status)
			key := cooldownKey{credential: c.fingerprint(), model: "limited-model"}
			deadline := e.cooldowns.entries[key].until
			clock.advance(30 * time.Second)
			for _, alias := range []string{"a", "alias", "a%28high%29", "a/variant/high", "overlay"} {
				engineFixtureStatus(t, execute(alias, c), status)
			}
			if limitedCalls.Load() != 1 {
				t.Fatal("alias or variant bypassed the cooldown")
			}
			if err := execute("b", c); err != nil || otherCalls.Load() != 1 {
				t.Fatalf("other model blocked: %v", err)
			}
			if err := e.configure(cfg); err != nil {
				t.Fatal(err)
			}
			engineFixtureStatus(t, execute("alias", c), status)
			if limitedCalls.Load() != 1 || !e.cooldowns.entries[key].until.Equal(deadline) {
				t.Fatal("reload reset or extended restriction")
			}
			if err := execute("b", c); err != nil || otherCalls.Load() != 2 {
				t.Fatalf("reload restricted unrelated model: %v", err)
			}
			other := c
			other.Key += "-other"
			engineFixtureStatus(t, execute("a", other), status)
			if limitedCalls.Load() != 2 {
				t.Fatal("explicit distinct credential inherited another credential's restriction")
			}
			clock.advance(30 * time.Second)
			engineFixtureStatus(t, execute("a", c), status)
			if limitedCalls.Load() != 3 {
				t.Fatal("reload or local retry extended original deadline")
			}
		})
	}
}
