package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

func TestProjectTurnsSteeringClosesPrevious(t *testing.T) {
	now := time.Now()
	meta := session.SessionMeta{ID: "s1", Agent: "calc", Status: session.StatusIdle}
	p1, p2 := "p_one", "p_two"
	run := "r_1"
	entries := []session.Entry{
		userEntry(1, p1, "first"),
		uiEvent(2, core.AgentEvent{Type: core.AgentStart, PromptID: p1, RunID: run}),
		uiEvent(3, core.AgentEvent{Type: core.TextDelta, PromptID: p1, RunID: run, DeltaText: "a"}),
		userEntry(4, p2, "steer"),
		uiEvent(5, core.AgentEvent{Type: core.TextDelta, PromptID: p2, RunID: run, DeltaText: "b"}),
		uiEvent(6, core.AgentEvent{Type: core.AgentEnd, PromptID: p2, RunID: run}),
	}
	_ = now
	got := projectTurns(meta, entries)
	if len(got.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(got.Turns))
	}
	if got.Turns[0].PromptID != p1 || got.Turns[0].Status != "complete" {
		t.Fatalf("turn0 = %+v, want complete p_one (closed by next user, not by agent_end)", got.Turns[0])
	}
	if got.Turns[1].PromptID != p2 || got.Turns[1].Status != "complete" {
		t.Fatalf("turn1 = %+v, want complete p_two (run ended)", got.Turns[1])
	}
	if got.Turns[0].User == nil || got.Turns[0].User.Text != "first" {
		t.Fatalf("turn0 user = %+v", got.Turns[0].User)
	}
	if len(got.Turns[0].Results) != 1 || len(got.Turns[1].Results) != 1 {
		t.Fatalf("results split = %d / %d", len(got.Turns[0].Results), len(got.Turns[1].Results))
	}
}

func TestProjectTurnsInterruptedCancelsOnlyLast(t *testing.T) {
	p1, p2 := "p_one", "p_two"
	run := "r_1"
	meta := session.SessionMeta{ID: "s1", Status: session.StatusInterrupted}
	entries := []session.Entry{
		userEntry(1, p1, "first"),
		uiEvent(2, core.AgentEvent{Type: core.TextDelta, PromptID: p1, RunID: run, DeltaText: "a"}),
		userEntry(3, p2, "steer"),
		uiEvent(4, core.AgentEvent{Type: core.TextDelta, PromptID: p2, RunID: run, DeltaText: "b"}),
		// no agent_end: crash / shutdown
	}
	got := projectTurns(meta, entries)
	if got.Turns[0].Status != "complete" {
		t.Fatalf("first turn = %s, want complete (closed by next user)", got.Turns[0].Status)
	}
	if got.Turns[1].Status != "cancelled" {
		t.Fatalf("last turn = %s, want cancelled", got.Turns[1].Status)
	}
}

func TestProjectTurnsCreatedAtFromEntries(t *testing.T) {
	t0 := time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)
	tThink := t0.Add(2 * time.Second)
	tOut := t0.Add(5 * time.Second)
	p1 := "p_one"
	run := "r_1"
	meta := session.SessionMeta{ID: "s1", Status: session.StatusIdle}
	user := userEntry(1, p1, "nihao")
	user.CreatedAt = t0
	think := uiEvent(2, core.AgentEvent{Type: core.ThinkingDelta, PromptID: p1, RunID: run, DeltaText: "..."})
	think.CreatedAt = tThink
	out := uiEvent(3, core.AgentEvent{Type: core.Output, PromptID: p1, RunID: run, OutputText: "hi"})
	out.CreatedAt = tOut
	got := projectTurns(meta, []session.Entry{user, think, out})
	if len(got.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(got.Turns))
	}
	tr := got.Turns[0]
	if tr.User == nil || !tr.User.CreatedAt.Equal(t0) {
		t.Fatalf("user.createdAt = %v, want %v", tr.User, t0)
	}
	if len(tr.Activity) != 1 || !tr.Activity[0].CreatedAt.Equal(tThink) {
		t.Fatalf("activity createdAt = %+v, want %v", tr.Activity, tThink)
	}
	if len(tr.Results) != 1 || !tr.Results[0].CreatedAt.Equal(tOut) {
		t.Fatalf("result createdAt = %+v, want %v", tr.Results, tOut)
	}
	if !tr.CreatedAt.Equal(tOut) {
		t.Fatalf("turn.createdAt = %v, want first result %v", tr.CreatedAt, tOut)
	}
}

func TestProjectTurnsCreatedAtFallsBackToActivity(t *testing.T) {
	t0 := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
	tThink := t0.Add(time.Minute)
	p1 := "p_one"
	meta := session.SessionMeta{ID: "s1", Status: session.StatusIdle}
	user := userEntry(1, p1, "nihao")
	user.CreatedAt = t0
	think := uiEvent(2, core.AgentEvent{Type: core.ThinkingDelta, PromptID: p1, DeltaText: "..."})
	think.CreatedAt = tThink
	got := projectTurns(meta, []session.Entry{user, think})
	if !got.Turns[0].CreatedAt.Equal(tThink) {
		t.Fatalf("turn.createdAt = %v, want last activity %v", got.Turns[0].CreatedAt, tThink)
	}
}

func TestProjectTurnsFailedOnOutputError(t *testing.T) {
	p1 := "p_one"
	run := "r_1"
	meta := session.SessionMeta{ID: "s1", Status: session.StatusIdle}
	entries := []session.Entry{
		userEntry(1, p1, "q"),
		uiEvent(2, core.AgentEvent{Type: core.OutputError, PromptID: p1, RunID: run, Error: "boom"}),
		uiEvent(3, core.AgentEvent{Type: core.AgentEnd, PromptID: p1, RunID: run}),
	}
	got := projectTurns(meta, entries)
	if len(got.Turns) != 1 || got.Turns[0].Status != "failed" {
		t.Fatalf("got %+v, want one failed turn", got.Turns)
	}
}

func TestProjectTurnsBudgetHintAndFailed(t *testing.T) {
	p1 := "p_one"
	run := "r_1"
	meta := session.SessionMeta{ID: "s1", Status: session.StatusIdle}
	entries := []session.Entry{
		userEntry(1, p1, "q"),
		uiEvent(2, core.AgentEvent{Type: core.BudgetExhausted, PromptID: p1, RunID: run}),
		uiEvent(3, core.AgentEvent{Type: core.AgentEnd, PromptID: p1, RunID: run}),
	}
	got := projectTurns(meta, entries)
	if got.Turns[0].Status != "failed" {
		t.Fatalf("status = %s, want failed", got.Turns[0].Status)
	}
	if len(got.Hints) != 1 || got.Hints[0].Type != core.BudgetExhausted {
		t.Fatalf("hints = %+v, want budget_exhausted at session level", got.Hints)
	}
}

func TestTurnsHTTPSteering(t *testing.T) {
	_, srv, _ := newTestGateway(t, 80*time.Millisecond,
		provider.FauxTurn{Text: "one"},
		provider.FauxTurn{Text: "two"},
	)
	base := srv.URL + "/api/agent/sessions/s1"
	code, body := postJSON(t, base+"/prompts", `{"text":"first"}`)
	if code != http.StatusAccepted {
		t.Fatalf("prompt: %d %+v", code, body)
	}
	p1, _ := body["prompt_id"].(string)
	time.Sleep(40 * time.Millisecond)
	code, body = postJSON(t, base+"/prompts", `{"text":"steer"}`)
	if code != http.StatusAccepted || body["status"] != "queued" {
		t.Fatalf("steer: %d %+v", code, body)
	}
	p2, _ := body["prompt_id"].(string)
	waitMessages(t, base+"/messages", 4)

	resp, err := http.Get(base + "/turns")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turns: %d", resp.StatusCode)
	}
	var got turnsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 2 {
		t.Fatalf("turns = %d %+v, want 2", len(got.Turns), got.Turns)
	}
	if got.Turns[0].PromptID != p1 || got.Turns[0].Status != "complete" {
		t.Fatalf("turn0 = %+v, want complete %s", got.Turns[0], p1)
	}
	if got.Turns[1].PromptID != p2 || got.Turns[1].Status != "complete" {
		t.Fatalf("turn1 = %+v, want complete %s", got.Turns[1], p2)
	}
}

func userEntry(seq int64, promptID, text string) session.Entry {
	m := core.NewUserMessage(text)
	m.PromptID = promptID
	payload, _ := json.Marshal(m)
	return session.Entry{Seq: seq, Type: session.EntryUser, Payload: payload}
}

func uiEvent(seq int64, ev core.AgentEvent) session.Entry {
	payload, _ := json.Marshal(ev)
	return session.Entry{Seq: seq, Type: session.EntryUIEvent, Payload: payload}
}
