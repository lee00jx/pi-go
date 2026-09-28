package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

func stampUserHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-User-ID"); u != "" {
			r = r.WithContext(ContextWithUserID(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

func doJSON(t *testing.T, method, url, body, user string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newAuthGateway(t *testing.T, opts ...Option) (*session.MemoryStore, *httptest.Server) {
	t.Helper()
	store := session.NewMemoryStore()
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "ok"})
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2,
	}
	opts = append([]Option{WithAuthMiddleware(stampUserHeader)}, opts...)
	srv := httptest.NewServer(New(store, runner, opts...))
	t.Cleanup(srv.Close)
	return store, srv
}

func TestCreateFailsClosedWithoutUser(t *testing.T) {
	_, srv := newAuthGateway(t)
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"user_id":"attacker"}`, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("create without user: %d %+v, want 401 (fail-closed)", code, body)
	}
}

func TestAnonymousUserFallbackIgnoresBodyUserID(t *testing.T) {
	store, srv := newAuthGateway(t, WithAnonymousUser("local"))
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"user_id":"attacker"}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v, want 201", code, body)
	}
	id, _ := body["id"].(string)
	meta, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.UserID != "local" {
		t.Fatalf("UserID = %q, want local (body user_id must be ignored)", meta.UserID)
	}
}

func TestFailClosedWithoutUser(t *testing.T) {
	_, srv := newAuthGateway(t)
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, body)
	}
	id, _ := body["id"].(string)
	base := srv.URL + "/api/agent/sessions/" + id

	if code, _ := doJSON(t, http.MethodPost, base+"/prompts", `{"text":"hi"}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("prompt without user: %d, want 401", code)
	}
	req, err := http.NewRequest(http.MethodGet, base+"/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("messages without user: %d, want 401", resp.StatusCode)
	}
}

func TestNoIdentityDoesNotLeakExistence(t *testing.T) {
	_, srv := newAuthGateway(t)
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, body)
	}
	id, _ := body["id"].(string)
	// Without an identity both the existing id and an unknown one must
	// answer 401 (never 404): the status code must not reveal which
	// session ids exist.
	for _, sid := range []string{id, "s_missing"} {
		if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions/"+sid+"/prompts", `{"text":"hi"}`, ""); code != http.StatusUnauthorized {
			t.Fatalf("prompt without user on %s: %d, want 401", sid, code)
		}
	}
}

func TestCreateUsesContextUser(t *testing.T) {
	store, srv := newAuthGateway(t)
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"user_id":"attacker"}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v, want 201", code, body)
	}
	id, _ := body["id"].(string)
	meta, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.UserID != "alice" {
		t.Fatalf("UserID = %q, want alice from context", meta.UserID)
	}
}

func TestSessionOwnership(t *testing.T) {
	_, srv := newAuthGateway(t)
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, body)
	}
	id, _ := body["id"].(string)
	base := srv.URL + "/api/agent/sessions/" + id

	if code, _ := doJSON(t, http.MethodPost, base+"/prompts", `{"text":"hi"}`, "bob"); code != http.StatusNotFound {
		t.Fatalf("bob prompt: %d, want 404", code)
	}
	req, err := http.NewRequest(http.MethodGet, base+"/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-ID", "bob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob messages: %d, want 404", resp.StatusCode)
	}

	code, body = doJSON(t, http.MethodPost, base+"/prompts", `{"text":"hi"}`, "alice")
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("alice prompt: %d %+v, want 202 started", code, body)
	}
}

// waitMessagesAs polls the messages endpoint as user until it returns n
// messages (the plain waitMessages helper sends no user and would get
// 401 under fail-closed).
func waitMessagesAs(t *testing.T, url, user string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err == nil {
			req.Header.Set("X-User-ID", user)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				var msgs []map[string]any
				_ = json.NewDecoder(resp.Body).Decode(&msgs)
				resp.Body.Close()
				if len(msgs) == n {
					return msgs
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d messages at %s", n, url)
	return nil
}

// TestAuthorizerOnlyWidensAccess: the custom authorizer may let admins
// through, but the default owner check still applies — alice (the owner)
// passes even though the authorizer would deny her, and bob is denied.
func TestAuthorizerOnlyWidensAccess(t *testing.T) {
	_, srv := newAuthGateway(t, WithAuthorizer(func(ctx context.Context, _ session.SessionMeta) error {
		if UserIDFromContext(ctx) == "admin" {
			return nil
		}
		return errForbidden
	}))
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, body)
	}
	id, _ := body["id"].(string)
	base := srv.URL + "/api/agent/sessions/" + id

	code, body = doJSON(t, http.MethodPost, base+"/prompts", `{"text":"hi"}`, "alice")
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("owner prompt: %d %+v, want 202 started (owner check must still run)", code, body)
	}
	waitMessagesAs(t, base+"/messages", "alice", 2)
	code, body = doJSON(t, http.MethodPost, base+"/prompts", `{"text":"hi"}`, "admin")
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("admin prompt: %d %+v, want 202 started (authorizer widens)", code, body)
	}
	if code, _ := doJSON(t, http.MethodPost, base+"/prompts", `{"text":"no"}`, "bob"); code != http.StatusNotFound {
		t.Fatalf("bob prompt: %d, want 404", code)
	}
}

func TestPromptAndRunIDs(t *testing.T) {
	_, srv, _ := newTestGateway(t, 300*time.Millisecond,
		provider.FauxTurn{Text: "turn1"},
		provider.FauxTurn{Text: "turn2"},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	code, body := postJSON(t, base+"/prompts", `{"text":"first"}`)
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("first: %d %+v, want 202 started", code, body)
	}
	p1, _ := body["prompt_id"].(string)
	runID, _ := body["run_id"].(string)
	if p1 == "" || runID == "" {
		t.Fatalf("started body = %+v, want prompt_id and run_id", body)
	}

	time.Sleep(60 * time.Millisecond)
	code, body = postJSON(t, base+"/prompts", `{"text":"steer"}`)
	if code != http.StatusAccepted || body["status"] != "queued" {
		t.Fatalf("steer: %d %+v, want 202 queued", code, body)
	}
	p2, _ := body["prompt_id"].(string)
	if p2 == "" || p2 == p1 {
		t.Fatalf("queued prompt_id = %q, want a new P", p2)
	}
	if _, ok := body["run_id"]; ok {
		t.Fatalf("queued body = %+v, must not bind a run_id yet", body)
	}

	msgs := waitMessages(t, base+"/messages", 4)
	if msgs[0]["promptId"] != p1 {
		t.Fatalf("user[0] promptId = %v, want %s", msgs[0]["promptId"], p1)
	}
	if msgs[2]["promptId"] != p2 {
		t.Fatalf("user[1] promptId = %v, want %s", msgs[2]["promptId"], p2)
	}

	frames, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var sawP2 bool
	for _, f := range frames {
		var ev core.AgentEvent
		if json.Unmarshal([]byte(f), &ev) != nil {
			continue
		}
		if ev.RunID != runID {
			t.Fatalf("event %s runId = %q, want the started run %q", ev.Type, ev.RunID, runID)
		}
		if ev.Type == core.TurnStart && ev.Turn == 0 {
			t.Fatalf("turn_start missing turn")
		}
		if ev.PromptID == p2 {
			sawP2 = true
		}
	}
	if !sawP2 {
		t.Fatal("steering prompt_id never appeared on the event stream")
	}
}

func TestResumeReturnsNewRunID(t *testing.T) {
	store, _, srv, _ := newRecoverFixture(t, []provider.FauxTurn{{Text: "continued"}})
	base := srv.URL + "/api/agent/sessions/s1"
	seedEntry(t, store, "s1", session.EntryUser, core.NewUserMessage("hello"))
	seedEntry(t, store, "s1", session.EntryAssistant, &core.Message{
		Role: core.RoleAssistant, Content: []core.Block{core.TextBlock("partial")},
	})
	meta, _ := store.GetSession(context.Background(), "s1")
	meta.Status = session.StatusInterrupted
	if err := store.UpdateSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}

	code, body := postJSON(t, base+"/resume", `{}`)
	if code != http.StatusOK || body["status"] != "resumed" {
		t.Fatalf("resume: %d %+v, want 200 resumed", code, body)
	}
	if body["run_id"] == "" {
		t.Fatalf("resume body = %+v, want a new run_id", body)
	}
}
