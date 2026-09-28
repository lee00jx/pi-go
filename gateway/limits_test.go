package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

func newSession(t *testing.T, store *session.MemoryStore, id string) {
	t.Helper()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: id, UserID: "u", Provider: "faux", Model: "m", Status: session.StatusIdle,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPromptRateLimit: more than maxPromptsPerMin prompts in a rolling minute
// from one user → 429 (DESIGN §7.5).
func TestPromptRateLimit(t *testing.T) {
	store := session.NewMemoryStore()
	newSession(t, store, "s1")
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "hi"})
	runner := &core.Runner{Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2}
	gw := New(store, runner, WithRateLimit(0, 2), WithAnonymousUser("u")) // 2 prompts/min
	srv := httptest.NewServer(gw)
	defer srv.Close()
	base := srv.URL + "/api/agent/sessions/s1"

	if code, _ := postJSON(t, base+"/prompts", `{"text":"1"}`); code != http.StatusAccepted {
		t.Fatalf("prompt 1: %d, want 202", code)
	}
	if code, _ := postJSON(t, base+"/prompts", `{"text":"2"}`); code != http.StatusAccepted {
		t.Fatalf("prompt 2: %d, want 202", code)
	}
	if code, _ := postJSON(t, base+"/prompts", `{"text":"3"}`); code != http.StatusTooManyRequests {
		t.Fatalf("prompt 3: %d, want 429", code)
	}
}

// TestConcurrentSessionLimit: a second in-flight run for the same user beyond
// maxConcurrent → 429, and the slot frees once the first run ends (DESIGN
// §7.5).
func TestConcurrentSessionLimit(t *testing.T) {
	store := session.NewMemoryStore()
	newSession(t, store, "s1")
	newSession(t, store, "s2")
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "hi"})
	// Delay every LLM call so a run stays in-flight across the two prompts.
	delayFn := func(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
		}
		return faux.Stream(ctx, req)
	}
	runner := &core.Runner{Store: store, StreamFn: delayFn, Model: core.Model{Provider: "faux", ID: "m"}, MaxTurns: 2}
	gw := New(store, runner, WithRateLimit(1, 0), WithAnonymousUser("u")) // 1 concurrent run/user
	srv := httptest.NewServer(gw)
	defer srv.Close()

	// s1 takes the user's single concurrency slot.
	if code, _ := postJSON(t, srv.URL+"/api/agent/sessions/s1/prompts", `{"text":"go"}`); code != http.StatusAccepted {
		t.Fatalf("s1 prompt: %d, want 202", code)
	}
	// s2 is a second concurrent run for the same user → 429.
	if code, _ := postJSON(t, srv.URL+"/api/agent/sessions/s2/prompts", `{"text":"go"}`); code != http.StatusTooManyRequests {
		t.Fatalf("s2 prompt at concurrency limit: %d, want 429", code)
	}
	// Once s1 finishes, the slot frees and s2 can start.
	waitMessages(t, srv.URL+"/api/agent/sessions/s1/messages", 2)
	if code, _ := postJSON(t, srv.URL+"/api/agent/sessions/s2/prompts", `{"text":"go"}`); code != http.StatusAccepted {
		t.Fatalf("s2 prompt after s1 finished: %d, want 202", code)
	}
}

// TestPromptIdempotency: a repeated request_id does not re-trigger a run; the
// second call reports "duplicate" and no new prompt/message is appended
// (DESIGN §7.3).
func TestPromptIdempotency(t *testing.T) {
	store := session.NewMemoryStore()
	newSession(t, store, "s1")
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "hi"})
	runner := &core.Runner{Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2}
	gw := New(store, runner, WithAnonymousUser("u"))
	srv := httptest.NewServer(gw)
	defer srv.Close()
	base := srv.URL + "/api/agent/sessions/s1"

	code, body := postJSON(t, base+"/prompts", `{"text":"hello","request_id":"r1"}`)
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("first: %d %+v, want 202 started", code, body)
	}
	waitMessages(t, base+"/messages", 2)

	code, body = postJSON(t, base+"/prompts", `{"text":"hello","request_id":"r1"}`)
	if code != http.StatusAccepted || body["status"] != "duplicate" {
		t.Fatalf("repeat: %d %+v, want 202 duplicate", code, body)
	}

	resp, err := http.Get(base + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msgs []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&msgs)
	if len(msgs) != 2 {
		t.Fatalf("messages after duplicate = %d, want 2 (no re-run)", len(msgs))
	}
}

// TestRequestDupNamespace: the prompt and confirmation namespaces share the
// per-session store but never collide, and repeats within a namespace are
// detected.
func TestRequestDupNamespace(t *testing.T) {
	gw := &Gateway{dedup: make(map[string]map[string]struct{})}
	if gw.requestDup("s1", "p:r1") {
		t.Fatal("first prompt r1 should not be a duplicate")
	}
	if !gw.requestDup("s1", "p:r1") {
		t.Fatal("repeat prompt r1 should be a duplicate")
	}
	if gw.requestDup("s1", "c:r1") {
		t.Fatal("confirmation r1 must not collide with prompt r1")
	}
	if !gw.requestDup("s1", "c:r1") {
		t.Fatal("repeat confirmation r1 should be a duplicate")
	}
	// A different session's same key is independent.
	if gw.requestDup("s2", "p:r1") {
		t.Fatal("same key in a different session should not be a duplicate")
	}
}
