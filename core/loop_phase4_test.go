package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// seedMsg appends one message entry to the store (test setup for history).
func seedMsg(t *testing.T, store *session.MemoryStore, sessionID string, typ session.EntryType, m *core.Message) {
	t.Helper()
	payload, _ := json.Marshal(m)
	if _, err := store.AppendEntry(context.Background(), sessionID, typ, payload, nil); err != nil {
		t.Fatal(err)
	}
}

// newContinueFixture builds a store with session s1 and a runner backed by a
// faux provider with the given turns, ready for RunContinue.
func newContinueFixture(t *testing.T, turns []provider.FauxTurn, tools ...core.Tool) (*session.MemoryStore, *core.Runner) {
	t.Helper()
	store := session.NewMemoryStore()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "u1", Provider: "faux", Model: "faux-mini",
	}); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "faux-mini", ContextWindow: 128000}, turns...)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Tools: tools,
		Model: faux.Model, MaxTurns: 8,
	}
	return store, runner
}

func drainEvents(t *testing.T, events <-chan core.AgentEvent) []core.AgentEvent {
	t.Helper()
	var got []core.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

// TestRunContinueDanglingRepair: a mid-turn interruption leaves the trailing
// assistant message with a tool call whose result was never persisted.
// RunContinue must synthesize an error result for it (so the context is
// valid for the provider) and then continue the loop — without executing the
// dangling call.
func TestRunContinueDanglingRepair(t *testing.T) {
	count := &countingTool{name: "count", mode: core.ModeParallel}
	store, runner := newContinueFixture(t,
		[]provider.FauxTurn{{Text: "continuing after interruption"}},
		count,
	)
	// Seed history: a user prompt, then an assistant message that asked for a
	// tool call — but the tool result is missing (the interruption point).
	user := core.NewUserMessage("do the thing")
	user.PromptID = "p_repair"
	seedMsg(t, store, "s1", session.EntryUser, user)
	seedMsg(t, store, "s1", session.EntryAssistant, &core.Message{
		Role:       core.RoleAssistant,
		Content:    []core.Block{core.ToolCallBlock("tc1", "count", nil)},
		StopReason: core.StopToolUse,
		PromptID:   "p_repair",
	})

	events, err := runner.RunContinue(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)

	// The dangling call must now have a synthesized error result in the store.
	entries, _ := store.ListEntries(context.Background(), "s1", 0, 0)
	var repaired bool
	for _, e := range entries {
		if e.Type != session.EntryToolResult {
			continue
		}
		var m core.Message
		if json.Unmarshal(e.Payload, &m) == nil && m.ToolCallID == "tc1" && m.IsError {
			repaired = true
			if m.PromptID != "p_repair" {
				t.Fatalf("repaired tool promptId = %q, want p_repair", m.PromptID)
			}
		}
	}
	if !repaired {
		t.Fatalf("dangling tool call tc1 was not repaired with an error result; entries=%d", len(entries))
	}
	// The dangling call must NOT have executed.
	count.mu.Lock()
	ran := count.calls
	count.mu.Unlock()
	if ran != 0 {
		t.Fatalf("dangling tool executed %d times, want 0 (repaired, not run)", ran)
	}
	// The loop continued and produced the scripted next turn, then ended.
	var sawContinueText, sawEnd bool
	for _, ev := range got {
		if ev.Type == core.MessageEnd && ev.Message != nil && ev.Message.Text() == "continuing after interruption" {
			sawContinueText = true
		}
		if ev.Type == core.AgentEnd {
			sawEnd = true
		}
	}
	if !sawContinueText || !sawEnd {
		t.Fatalf("continue text=%v end=%v, want both", sawContinueText, sawEnd)
	}
}

// TestRunContinueCleanTail: when the history already ends cleanly (no dangling
// tool calls), RunContinue must not synthesize anything and just continue.
func TestRunContinueCleanTail(t *testing.T) {
	store, runner := newContinueFixture(t,
		[]provider.FauxTurn{{Text: "all done"}},
	)
	seedMsg(t, store, "s1", session.EntryUser, core.NewUserMessage("hi"))
	seedMsg(t, store, "s1", session.EntryAssistant, &core.Message{
		Role: core.RoleAssistant, Content: []core.Block{core.TextBlock("hello there")},
	})

	events, err := runner.RunContinue(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)

	// No tool results should have been synthesized (clean tail).
	entries, _ := store.ListEntries(context.Background(), "s1", 0, 0)
	for _, e := range entries {
		if e.Type == session.EntryToolResult {
			t.Fatalf("unexpected tool_result synthesized on a clean tail: %+v", e)
		}
	}
	var sawText, sawEnd bool
	for _, ev := range got {
		if ev.Type == core.MessageEnd && ev.Message != nil && ev.Message.Text() == "all done" {
			sawText = true
		}
		if ev.Type == core.AgentEnd {
			sawEnd = true
		}
	}
	if !sawText || !sawEnd {
		t.Fatalf("continue text=%v end=%v, want both", sawText, sawEnd)
	}
}

// TestRunContinueNoHistory: continuing a session with no messages is an error.
func TestRunContinueNoHistory(t *testing.T) {
	_, runner := newContinueFixture(t, []provider.FauxTurn{{Text: "x"}})
	if _, err := runner.RunContinue(context.Background(), "s1"); err == nil {
		t.Fatal("RunContinue on an empty session must return an error")
	}
}
