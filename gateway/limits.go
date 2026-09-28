package gateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errRateLimited is returned by startLoop when a run would exceed the user's
// concurrency limit (DESIGN §7.5); the handler maps it to HTTP 429.
var errRateLimited = errors.New("gateway: user rate limit exceeded")

// ctxUserIDKey lets the integrator's auth middleware stamp the authenticated
// user into the request context (DESIGN §7.5). Without it the gateway falls
// back to the session's UserID, then "default".
type ctxUserIDKey struct{}

// ContextWithUserID returns a context carrying the authenticated user id.
// The integrator's JWT middleware calls it after authenticating the request
// so session ownership and rate limiting use the real tenant, not a
// client-supplied body field.
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxUserIDKey{}, userID)
}

// UserIDFromContext returns the authenticated user stamped by
// ContextWithUserID, or "" when none was injected.
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxUserIDKey{}).(string); ok && v != "" {
		return v
	}
	return ""
}

// limiter is the in-memory multi-tenant rate limiter (DESIGN §7.5): a
// per-user concurrent-run cap plus a per-user prompts-per-minute sliding
// window. A nil limiter (no WithRateLimit option) disables both dimensions,
// which is the lenient default for single-tenant deployments.
type limiter struct {
	mu               sync.Mutex
	maxConcurrent    int
	maxPromptsPerMin int
	running          map[string]int
	prompts          map[string][]time.Time
}

func newLimiter(maxConcurrent, maxPromptsPerMin int) *limiter {
	return &limiter{
		maxConcurrent:    maxConcurrent,
		maxPromptsPerMin: maxPromptsPerMin,
		running:          make(map[string]int),
		prompts:          make(map[string][]time.Time),
	}
}

// allowPrompt reports whether user may send a prompt now; if so it records
// the timestamp in a rolling 60s window.
func (l *limiter) allowPrompt(user string) bool {
	if l == nil || l.maxPromptsPerMin <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	kept := l.prompts[user][:0]
	for _, t := range l.prompts[user] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.maxPromptsPerMin {
		l.prompts[user] = kept
		return false
	}
	l.prompts[user] = append(kept, now)
	return true
}

// allowConcurrent reports whether user may start another run now; if so it
// counts it. The caller must balance it with releaseConcurrent exactly once
// per allowed run (whether the run finishes or fails to start).
func (l *limiter) allowConcurrent(user string) bool {
	if l == nil || l.maxConcurrent <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[user] >= l.maxConcurrent {
		return false
	}
	l.running[user]++
	return true
}

func (l *limiter) releaseConcurrent(user string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[user] > 0 {
		l.running[user]--
	}
}

// requestRollback removes a claim made by requestDup (#7 fix). Failure
// paths (rate limited, store error, shutting down) must not leave the
// key behind: a client that received a 4xx/5xx may retry with the same
// request_id, and the retry must not be swallowed as a duplicate of a
// request that never ran.
func (g *Gateway) requestRollback(sessionID, key string) {
	g.dedupMu.Lock()
	defer g.dedupMu.Unlock()
	if m := g.dedup[sessionID]; m != nil {
		delete(m, key)
	}
}

// maxDedupPerSession bounds the in-memory idempotency window per session
// (DESIGN §7.3). Idempotency is best-effort: when a session's window overflows
// the oldest tracking is dropped wholesale rather than growing without bound.
const maxDedupPerSession = 1000

// requestDup atomically reports whether (session, key) was already seen and,
// if not, marks it. The key is the client request_id namespaced by caller
// ("p:" for prompts, "c:" for confirmations) so the two never collide. A
// duplicate gets back the fact that it was already accepted — the events it
// triggered are already in the replayable SSE stream (DESIGN §7.3).
func (g *Gateway) requestDup(sessionID, key string) bool {
	g.dedupMu.Lock()
	defer g.dedupMu.Unlock()
	m := g.dedup[sessionID]
	if m == nil {
		m = make(map[string]struct{})
		g.dedup[sessionID] = m
	}
	if _, ok := m[key]; ok {
		return true
	}
	if len(m) >= maxDedupPerSession {
		for k := range m {
			delete(m, k)
		}
	}
	m[key] = struct{}{}
	return false
}
