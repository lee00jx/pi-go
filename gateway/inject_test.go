package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

func newInjectFixture(t *testing.T) (*Gateway, *session.MemoryStore, context.Context) {
	t.Helper()
	store := session.NewMemoryStore()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "alice", Provider: "faux", Model: "m",
		Status: session.StatusIdle, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "ok"})
	runner := &core.Runner{Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2}
	gw := New(store, runner, WithAnonymousUser("alice"))
	ctx := ContextWithUserID(context.Background(), "alice")
	return gw, store, ctx
}

func TestInjectTurnAppearsInTurns(t *testing.T) {
	gw, _, ctx := newInjectFixture(t)
	data := json.RawMessage(`{"resultId":"r1","type":"calculation_result"}`)
	pid, err := gw.InjectTurn(ctx, "s1", InjectTurnInput{
		UserText:      "开始测算",
		AssistantText: "已生成测算摘要",
		OutputText:    "测算摘要（短）",
		OutputKind:    "summary",
		OutputData:    data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pid == "" {
		t.Fatal("empty prompt_id")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agent/sessions/s1/turns", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("turns: %d %s", rec.Code, rec.Body.String())
	}
	var got turnsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(got.Turns))
	}
	tr := got.Turns[0]
	if tr.PromptID != pid || tr.Status != "complete" {
		t.Fatalf("turn = %+v, want complete %s", tr, pid)
	}
	if tr.User == nil || tr.User.Text != "开始测算" {
		t.Fatalf("user = %+v", tr.User)
	}
	if len(tr.Results) != 1 || tr.Results[0].Type != core.Output {
		t.Fatalf("results = %+v, want one output", tr.Results)
	}
	if tr.Results[0].OutputKind != "summary" || tr.Results[0].OutputText != "测算摘要（短）" {
		t.Fatalf("output fields = %+v", tr.Results[0])
	}
	if tr.User.CreatedAt.IsZero() || tr.Results[0].CreatedAt.IsZero() || tr.CreatedAt.IsZero() {
		t.Fatalf("createdAt missing: user=%v result=%v turn=%v", tr.User.CreatedAt, tr.Results[0].CreatedAt, tr.CreatedAt)
	}
	if string(tr.Results[0].OutputData) != string(data) {
		t.Fatalf("OutputData = %s, want %s", tr.Results[0].OutputData, data)
	}
}

func TestInjectTurnSecondClosesPrevious(t *testing.T) {
	gw, _, ctx := newInjectFixture(t)
	p1, err := gw.InjectTurn(ctx, "s1", InjectTurnInput{
		UserText: "one", AssistantText: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := gw.InjectTurn(ctx, "s1", InjectTurnInput{
		UserText: "two", AssistantText: "a2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("prompt ids collided: %s", p1)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agent/sessions/s1/turns", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	var got turnsResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(got.Turns))
	}
	if got.Turns[0].PromptID != p1 || got.Turns[0].Status != "complete" {
		t.Fatalf("turn0 = %+v", got.Turns[0])
	}
	if got.Turns[1].PromptID != p2 || got.Turns[1].Status != "complete" {
		t.Fatalf("turn1 = %+v", got.Turns[1])
	}
}

func TestInjectTurnRejectsRunning(t *testing.T) {
	gw, store, ctx := newInjectFixture(t)
	meta, _ := store.GetSession(ctx, "s1")
	meta.Status = session.StatusRunning
	if err := store.UpdateSession(ctx, meta); err != nil {
		t.Fatal(err)
	}
	_, err := gw.InjectTurn(ctx, "s1", InjectTurnInput{
		UserText: "x", AssistantText: "y",
	})
	if !errors.Is(err, ErrSessionRunning) {
		t.Fatalf("err = %v, want ErrSessionRunning", err)
	}
}

func TestInjectTurnAuth(t *testing.T) {
	store := session.NewMemoryStore()
	_ = store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "alice", Provider: "faux", Model: "m", Status: session.StatusIdle,
	})
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "ok"})
	runner := &core.Runner{Store: store, StreamFn: faux.Stream, Model: faux.Model}
	strict := New(store, runner) // fail-closed, no anon

	_, err := strict.InjectTurn(context.Background(), "s1", InjectTurnInput{
		UserText: "x", AssistantText: "y",
	})
	if !errors.Is(err, errNoUser) {
		t.Fatalf("no user: %v, want errNoUser", err)
	}

	bob := ContextWithUserID(context.Background(), "bob")
	_, err = strict.InjectTurn(bob, "s1", InjectTurnInput{
		UserText: "x", AssistantText: "y",
	})
	if !errors.Is(err, errForbidden) {
		t.Fatalf("non-owner: %v, want errForbidden", err)
	}

	alice := ContextWithUserID(context.Background(), "alice")
	if _, err := strict.InjectTurn(alice, "s1", InjectTurnInput{
		UserText: "x", AssistantText: "y",
	}); err != nil {
		t.Fatalf("owner: %v", err)
	}
}

func TestInjectTurnOutputTextFallback(t *testing.T) {
	gw, store, ctx := newInjectFixture(t)
	pid, err := gw.InjectTurn(ctx, "s1", InjectTurnInput{
		UserText: "q", OutputText: "only output",
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := store.ListEntries(ctx, "s1", 0, 0)
	var sawAsst bool
	for _, e := range entries {
		if e.Type != session.EntryAssistant {
			continue
		}
		var m core.Message
		_ = json.Unmarshal(e.Payload, &m)
		if m.PromptID == pid && m.Text() == "only output" {
			sawAsst = true
		}
	}
	if !sawAsst {
		t.Fatal("assistant entry should fall back to OutputText for buildContext")
	}
}
