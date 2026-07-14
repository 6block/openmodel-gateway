package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

const defaultSessionTTL = 10 * time.Minute

type sessionEntry struct {
	workerID string
	expiry   time.Time
}

// sessionAffinity maps a session key to the worker that last served it, so the
// turns of one conversation stick to the same worker and reuse vLLM's prefix
// cache. Affinity is a PREFERENCE, not a pin: if the sticky worker is gone or
// not routable the caller falls back to normal weighted routing.
type sessionAffinity struct {
	mu  sync.Mutex
	m   map[string]sessionEntry
	ttl time.Duration
}

func newSessionAffinity(ttl time.Duration) *sessionAffinity {
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	return &sessionAffinity{m: make(map[string]sessionEntry), ttl: ttl}
}

// get returns the worker mapped to key if present and unexpired.
func (s *sessionAffinity) get(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiry) {
		delete(s.m, key)
		return "", false
	}
	return e.workerID, true
}

// put records (or refreshes) the worker for a session key, and opportunistically
// evicts a bounded number of expired entries to cap memory.
func (s *sessionAffinity) put(key, workerID string) {
	if key == "" || workerID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = sessionEntry{workerID: workerID, expiry: time.Now().Add(s.ttl)}
	now := time.Now()
	scanned := 0
	for k, e := range s.m {
		if now.After(e.expiry) {
			delete(s.m, k)
		}
		if scanned++; scanned >= 64 {
			break
		}
	}
}

// shortSessionHash returns an opaque, comparable fingerprint of a session key for
// observability (the X-Session-Key response header) — it lets a client/operator
// verify that the same (api key, session id) maps to one key and that two different
// api keys with the same X-Session-Id map to DIFFERENT keys, without exposing the
// raw key (which embeds the api key name). Empty in, empty out.
func shortSessionHash(sessKey string) string {
	if sessKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessKey))
	return hex.EncodeToString(sum[:8])
}

// sessionKeyOf derives a stable session id: an explicit X-Session-Id header if
// present, else a hash of the API key + the first one or two messages (stable
// across a conversation's turns, so they map to the same worker). Returns "" if
// neither is available (e.g. an empty body) — then routing is not made sticky.
func sessionKeyOf(apiKeyName, header string, body []byte) string {
	if header != "" {
		// Namespace by API key so the same X-Session-Id under two different keys
		// does NOT share a routing slot (matches the message-prefix path below).
		return "h:" + apiKeyName + "\x00" + header
	}
	prefix := messagesPrefix(body)
	if prefix == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKeyName + "\x00" + prefix))
	return "m:" + hex.EncodeToString(sum[:16])
}

// messagesPrefix returns the first up-to-two messages (role+content) as a stable
// conversation fingerprint, or the prompt for /v1/completions.
func messagesPrefix(body []byte) string {
	var p struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Prompt any `json:"prompt"`
	}
	if json.Unmarshal(body, &p) != nil {
		return ""
	}
	out := ""
	for i, m := range p.Messages {
		if i >= 2 {
			break
		}
		if s, ok := m.Content.(string); ok {
			out += m.Role + ":" + s + "\n"
		}
	}
	if out != "" {
		return out
	}
	if s, ok := p.Prompt.(string); ok && s != "" {
		return "prompt:" + s
	}
	return ""
}
