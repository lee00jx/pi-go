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

func newAgentGateway(t *testing.T) (*session.MemoryStore, *httptest.Server) {
	t.Helper()
	store := session.NewMemoryStore()
	mk := func(system string) *core.Runner {
		faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: system})
		return &core.Runner{
			Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2,
			SystemPrompt: system,
		}
	}
	calc := mk("calc")
	compare := mk("compare")
	srv := httptest.NewServer(New(store, calc,
		WithAgentRunner("calc", calc),
		WithAgentRunner("compare", compare),
		WithAuthMiddleware(stampUserHeader),
	))
	t.Cleanup(srv.Close)
	return store, srv
}

func TestCreateRequiresRegisteredAgent(t *testing.T) {
	store, srv := newAgentGateway(t)
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{}`, "alice")
	if code != http.StatusBadRequest {
		t.Fatalf("missing agent: %d %+v, want 400", code, body)
	}
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"agent":"nope"}`, "alice")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d %+v, want 400", code, body)
	}
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"agent":"calc"}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("calc: %d %+v, want 201", code, body)
	}
	id, _ := body["id"].(string)
	meta, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Agent != "calc" || meta.UserID != "alice" {
		t.Fatalf("meta = %+v, want agent=calc user=alice", meta)
	}
}

func TestListSessionsFiltersByAgentAndUser(t *testing.T) {
	_, srv := newAgentGateway(t)
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/agent/sessions", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("list without user: %d, want 401", code)
	}
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"agent":"calc","title":"c1"}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create calc: %d %+v", code, body)
	}
	calcID, _ := body["id"].(string)
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"agent":"compare"}`, "alice")
	if code != http.StatusCreated {
		t.Fatalf("create compare: %d %+v", code, body)
	}
	if code, _ = doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions", `{"agent":"calc"}`, "bob"); code != http.StatusCreated {
		t.Fatalf("bob create: %d", code)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/agent/sessions?agent=calc", nil)
	req.Header.Set("X-User-ID", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["id"] != calcID || list[0]["agent"] != "calc" {
		t.Fatalf("alice calc list = %+v, want [%s]", list, calcID)
	}
}

func TestPromptUnknownAgentReturns503(t *testing.T) {
	store := session.NewMemoryStore()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "alice", Agent: "calc", Provider: "faux", Model: "m",
		Status: session.StatusIdle, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "ok"})
	onlyCompare := &core.Runner{Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2}
	srv := httptest.NewServer(New(store, onlyCompare,
		WithAgentRunner("compare", onlyCompare),
		WithAuthMiddleware(stampUserHeader),
		WithAnonymousUser("alice"),
	))
	t.Cleanup(srv.Close)

	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/agent/sessions/s1/prompts", `{"text":"hi"}`, "alice")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("prompt missing runner: %d %+v, want 503", code, body)
	}
}

func TestResumeStampsLastUserPromptID(t *testing.T) {
	store, _, srv, _ := newRecoverFixture(t, []provider.FauxTurn{{Text: "continued"}})
	base := srv.URL + "/api/agent/sessions/s1"
	user := core.NewUserMessage("hello")
	user.PromptID = "p_resume"
	seedEntry(t, store, "s1", session.EntryUser, user)
	seedEntry(t, store, "s1", session.EntryAssistant, &core.Message{
		Role: core.RoleAssistant, Content: []core.Block{core.TextBlock("partial")},
	})
	meta, _ := store.GetSession(context.Background(), "s1")
	meta.Status = session.StatusInterrupted
	if err := store.UpdateSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}

	code, body := postJSON(t, base+"/resume", `{}`)
	if code != http.StatusOK {
		t.Fatalf("resume: %d %+v", code, body)
	}
	if _, ok := body["prompt_id"]; ok {
		t.Fatalf("resume must not mint a prompt_id: %+v", body)
	}
	frames, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, f := range frames {
		var ev core.AgentEvent
		if json.Unmarshal([]byte(f), &ev) != nil {
			continue
		}
		if ev.Type == core.TextDelta && ev.PromptID == "p_resume" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("resume events were not stamped with the last user prompt_id")
	}
}
