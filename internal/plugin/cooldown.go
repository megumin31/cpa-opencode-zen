package plugin

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/megumin31/cpa-opencode-zen/internal/catalog"
)

// The ABI cannot attach headers to an RPC error. Gate each credential and model:
// HTTP 429 uses a fixed three minutes; HTTP 503 retains upstream Retry-After.
// Neither 429 nor 503 alone establishes an account-wide restriction. No sleeps,
// credential rotation, or inference retries are performed by this plugin.
type cooldownStore struct {
	sync.Mutex
	entries map[cooldownKey]cooldown
	now     func() time.Time
}

// Keep the two dimensions separate; aliases and request-local reasoning options
// never become part of a cooldown identity.
type cooldownKey struct {
	credential string
	model      string
}

// requestCooldownKey uses the final wire model, after all configured overlays.
// The resolver has already removed public aliases and variant labels. Gemini's
// model is carried in the URL, not a body overlay; its optional models/ prefix
// is removed by Prepare. Preserve actual model spelling rather than guessing
// that case differences or suffixes in real upstream IDs are interchangeable.
func requestCooldownKey(c credential, r catalog.Route, payload []byte) (cooldownKey, error) {
	model := strings.TrimPrefix(r.ModelID, "models/")
	if r.Protocol != "gemini" {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.Model == "" {
			return cooldownKey{}, fail(400, "upstream request must name a model")
		}
		model = body.Model
	}
	if model == "" {
		return cooldownKey{}, fail(400, "upstream request must name a model")
	}
	return cooldownKey{credential: c.fingerprint(), model: model}, nil
}

type cooldown struct {
	until  time.Time
	status int
}

const rateLimitCooldown = 3 * time.Minute

// A real HTTP 429 always starts a fixed cooldown, even without valid headers.
// Only HTTP 503 uses upstream retry timing (milliseconds take precedence).
func (s *cooldownStore) rememberHeaders(key cooldownKey, status int, headers http.Header) {
	if status == http.StatusTooManyRequests {
		s.remember(key, status, s.now().Add(rateLimitCooldown))
		return
	}
	if status != http.StatusServiceUnavailable {
		return
	}
	if value := strings.TrimSpace(headers.Get("Retry-After-Ms")); value != "" {
		if ms, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(ms) && !math.IsInf(ms, 0) && ms >= 0 {
			max := float64(math.MaxInt64 / int64(time.Millisecond))
			delay := time.Duration(math.MaxInt64)
			if ms < max-1 {
				delay = time.Duration(ms * float64(time.Millisecond))
			}
			s.remember(key, status, s.now().Add(delay))
			return
		}
	}
	s.rememberAfter(key, status, headers.Get("Retry-After"))
}

func (s *cooldownStore) rememberAfter(key cooldownKey, status int, value string) {
	if status == http.StatusTooManyRequests {
		s.remember(key, status, s.now().Add(rateLimitCooldown))
		return
	}
	if status != http.StatusServiceUnavailable {
		return
	}
	value = strings.TrimSpace(value)
	current := s.now()
	var until time.Time
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		// Saturate rather than overflowing; retain the server's requested restriction.
		if seconds > int64((time.Duration(1<<63-1))/time.Second) {
			seconds = int64((time.Duration(1<<63 - 1)) / time.Second)
		}
		until = current.Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(value); err == nil {
		until = date
	} else {
		return
	}
	s.remember(key, status, until)
}

func (s *cooldownStore) remember(key cooldownKey, status int, until time.Time) {
	current := s.now()
	if !until.After(current) {
		return
	}
	s.Lock()
	defer s.Unlock()
	for key, old := range s.entries {
		if !old.until.After(current) {
			delete(s.entries, key)
		}
	}
	if previous := s.entries[key]; previous.until.Before(until) {
		s.entries[key] = cooldown{until, status}
	}
}
func (s *cooldownStore) check(key cooldownKey) error {
	s.Lock()
	defer s.Unlock()
	item, ok := s.entries[key]
	if !ok {
		return nil
	}
	if !item.until.After(s.now()) {
		delete(s.entries, key)
		return nil
	}
	if item.status == http.StatusTooManyRequests {
		return fail(item.status, "OpenCode three-minute rate-limit cooldown is still active for this model")
	}
	return fail(item.status, "OpenCode retry-after interval is still active for this model")
}
