package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// newIdempotencyGateway: one session, one faux turn, rate limiter at
// 1 prompt/min so the second distinct prompt trips 429.
func newIdempotencyGateway(t *testing.T) *httptest.Server {
	t.Helper()
	store := session.NewMemoryStore()
	meta := session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m", Status: session.StatusIdle, CreatedAt: time.Now()}
	if err := store.CreateSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "ok"})
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream,
		Model: faux.Model, MaxTurns: 8,
	}
	gw := New(store, runner, WithRateLimit(0, 1), WithAnonymousUser("u"))
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv
}

// A rate-limited prompt (429) must roll back its idempotency claim (#7 fix):
// the client may retry with the same request_id, and the retry must NOT
// be swallowed as a duplicate of a request that never ran. Regression:
// the old handlePrompt marked the key before the rate check.
func TestPromptIdempotencyRetryAfter429(t *testing.T) {
	srv := newIdempotencyGateway(t)
	base := srv.URL + "/api/agent/sessions/s1"

	// A is accepted and stays marked.
	code, body := postJSON(t, base+"/prompts", `{"text":"a","request_id":"r1"}`)
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("first prompt: %d %+v, want 202 started", code, body)
	}
	// B trips the rate limit.
	code, body = postJSON(t, base+"/prompts", `{"text":"b","request_id":"r2"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited prompt: %d, want 429", code)
	}
	// Retrying B with the same request_id must hit the limiter again —
	// not come back as a 202 duplicate.
	code, body = postJSON(t, base+"/prompts", `{"text":"b","request_id":"r2"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("retry after 429: %d %+v, want 429 again (rollback)", code, body)
	}
	// Retrying A with the same request_id IS a duplicate.
	code, body = postJSON(t, base+"/prompts", `{"text":"a","request_id":"r1"}`)
	if code != http.StatusAccepted || body["status"] != "duplicate" {
		t.Fatalf("duplicate prompt: %d %+v, want 202 duplicate", code, body)
	}
}

// A confirmation POST that lands on nothing pending (404) must roll back
// its idempotency claim too: a retry with the same request_id must get the
// same 404, not a 200 duplicate of an answer that was never delivered.
func TestConfirmIdempotencyRollbackOn404(t *testing.T) {
	srv := newIdempotencyGateway(t)
	base := srv.URL + "/api/agent/sessions/s1"

	code, _ := postJSON(t, base+"/confirmations", `{"id":"c1","decision":"allow","request_id":"rc1"}`)
	if code != http.StatusNotFound {
		t.Fatalf("confirm without pending: %d, want 404", code)
	}
	code, body := postJSON(t, base+"/confirmations", `{"id":"c1","decision":"allow","request_id":"rc1"}`)
	if code != http.StatusNotFound {
		t.Fatalf("confirm retry after 404: %d %+v, want 404 again (rollback)", code, body)
	}
}
