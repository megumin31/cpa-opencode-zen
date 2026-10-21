package plugin

import (
	"net/http"
	"testing"
	"time"

	"github.com/megumin31/cpa-opencode-zen/internal/fault"
)

func TestRetryAfterGate(t *testing.T) {
	engine := newTestEngine(t)

	saved := engine.now
	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return current }
	defer func() { engine.now = saved }()
	c := credential{Key: "cooldown-fake", Server: "https://opencode.ai/console"}
	engine.cooldowns.rememberAfter(cooldownKey{credential: c.fingerprint(), model: "m"}, 429, "120")
	if e := engine.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}); e == nil || e.(*fault.Error).Status != 429 {
		t.Fatalf("gate error %v", e)
	}
	other := c
	other.Key = "other-fake"
	if e := engine.cooldowns.check(cooldownKey{credential: other.fingerprint(), model: "m"}); e != nil {
		t.Fatal("cross credential restriction")
	}
	current = current.Add(180 * time.Second)
	if e := engine.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}); e != nil {
		t.Fatal(e)
	}
	engine.cooldowns.rememberAfter(cooldownKey{credential: c.fingerprint(), model: "m"}, 503, current.Add(time.Hour).Format(http.TimeFormat))
	if e := engine.cooldowns.check(cooldownKey{credential: c.fingerprint(), model: "m"}); e == nil || e.(*fault.Error).Status != 503 {
		t.Fatal(e)
	}
}
