package plugin

import (
	"net/http"
	"testing"
	"time"
)

func Test429AlwaysUsesThreeMinutes(t *testing.T) {
	for _, headers := range []http.Header{
		nil, {}, {"Retry-After": {"30118"}}, {"Retry-After": {"0"}},
		{"Retry-After": {"invalid"}}, {"Retry-After": {"-1"}},
		{"Retry-After": {"Fri, 01 Jan 2100 00:00:00 GMT"}},
		{"Retry-After-Ms": {"1.5"}, "Retry-After": {"999999999999999999999"}},
		{"Retry-After-Ms": {"NaN"}},
	} {
		clock := newEngineFixtureClock(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
		s := cooldownStore{entries: make(map[cooldownKey]cooldown), now: clock.now}
		key := cooldownKey{credential: "fixture", model: "model-a"}
		s.rememberHeaders(key, 429, headers)
		deadline := clock.now().Add(3 * time.Minute)
		if got := s.entries[key].until; !got.Equal(deadline) {
			t.Fatalf("headers=%v deadline=%v want %v", headers, got, deadline)
		}
		clock.advance(179 * time.Second)
		for range 3 {
			engineFixtureStatus(t, s.check(key), 429)
		}
		if !s.entries[key].until.Equal(deadline) {
			t.Fatal("local rejection extended deadline")
		}
		if err := s.check(cooldownKey{credential: "fixture", model: "model-b"}); err != nil {
			t.Fatal("cross-model gate", err)
		}
		clock.advance(time.Second)
		if err := s.check(key); err != nil {
			t.Fatal("not released at three minutes", err)
		}
		s.rememberHeaders(key, 429, nil)
		if !s.entries[key].until.Equal(deadline.Add(3 * time.Minute)) {
			t.Fatal("new upstream429 did not start fresh window")
		}
	}
}
