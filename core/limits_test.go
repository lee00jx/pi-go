package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// makeFauxStore builds a memory store with session s1 (provider "faux").
func makeFauxStore(t *testing.T) *session.MemoryStore {
	t.Helper()
	store := session.NewMemoryStore()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "u1", Provider: "faux", Model: "faux-mini",
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func bigUsage(in, out int) *core.Usage {
	return &core.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}
}

// drainWithTimeout drains events, failing the test if they never complete —
// the guard that turns a hung run into a fast test failure instead of a
// 10-minute CI stall.
func drainWithTimeout(t *testing.T, events <-chan core.AgentEvent, d time.Duration) []core.AgentEvent {
	t.Helper()
	ch := make(chan []core.AgentEvent, 1)
	go func() {
		var got []core.AgentEvent
		for ev := range events {
			got = append(got, ev)
		}
		ch <- got
	}()
	select {
	case got := <-ch:
		return got
	case <-time.After(d):
		t.Fatalf("events did not complete within %v (run hung)", d)
	}
	return nil
}

// With a non-terminating tool the loop would run every scripted turn; the
// per-session token budget stops it the moment cumulative usage crosses the
// limit, emits budget_exhausted, and skips the over-budget turn's tool calls.
func TestTokenBudgetStops(t *testing.T) {
	store := makeFauxStore(t)
	count := &countingTool{name: "count", mode: core.ModeParallel}
	// 3 turns, each 100 in / 100 out. Budget 250: turn 1 fits (cum 200),
	// turn 2 crosses (cum 400) → stop there; turn 3 never runs.
	turns := []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "count"}}, Usage: bigUsage(100, 100)},
		{ToolCalls: []provider.FauxToolCall{{Name: "count"}}, Usage: bigUsage(100, 100)},
		{ToolCalls: []provider.FauxToolCall{{Name: "count"}}, Usage: bigUsage(100, 100)},
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m", ContextWindow: 100000}, turns...)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Tools: []core.Tool{count},
		Model: faux.Model, MaxTurns: 10, TokenBudget: 250,
	}
	events, err := runner.Run(context.Background(), "s1", []*core.Message{core.NewUserMessage("go")})
	if err != nil {
		t.Fatal(err)
	}
	got := drainWithTimeout(t, events, 5*time.Second)

	var sawBudget, sawEnd bool
	asstMsgs := 0
	for _, ev := range got {
		switch ev.Type {
		case core.BudgetExhausted:
			sawBudget = true
		case core.AgentEnd:
			sawEnd = true
		case core.MessageEnd:
			if ev.Message != nil && ev.Message.Role == core.RoleAssistant {
				asstMsgs++
			}
		}
	}
	if !sawBudget || !sawEnd {
		t.Fatalf("budget=%v end=%v, want both", sawBudget, sawEnd)
	}
	if asstMsgs != 2 {
		t.Fatalf("assistant messages = %d, want 2 (stopped at the over-budget turn)", asstMsgs)
	}
	count.mu.Lock()
	calls := count.calls
	count.mu.Unlock()
	if calls != 1 {
		t.Fatalf("tool executed %d times, want 1 (over-budget turn's call is skipped)", calls)
	}
	// The two completed turns' usage was folded into the session header.
	meta, _ := store.GetSession(context.Background(), "s1")
	if meta.TokensIn != 200 || meta.TokensOut != 200 {
		t.Fatalf("session usage = in %d out %d, want 200/200", meta.TokensIn, meta.TokensOut)
	}
}

// Per-turn usage is accumulated into the session header and priced via
// CostPerMillion (DESIGN §7.6).
func TestUsageAccumulatedWithCost(t *testing.T) {
	store := makeFauxStore(t)
	count := &countingTool{name: "count", mode: core.ModeParallel}
	turns := []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "count"}}, Usage: bigUsage(1000, 500)},
		{Text: "done", Usage: bigUsage(1000, 500)},
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m", ContextWindow: 100000}, turns...)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Tools: []core.Tool{count},
		Model: faux.Model, MaxTurns: 4,
		CostPerMillion: func(core.Model) (in, out float64) { return 2.0, 4.0 },
	}
	events, err := runner.Run(context.Background(), "s1", []*core.Message{core.NewUserMessage("go")})
	if err != nil {
		t.Fatal(err)
	}
	drainWithTimeout(t, events, 5*time.Second)

	meta, _ := store.GetSession(context.Background(), "s1")
	if meta.TokensIn != 2000 || meta.TokensOut != 1000 {
		t.Fatalf("session usage = in %d out %d, want 2000/1000", meta.TokensIn, meta.TokensOut)
	}
	// cost = 2 × (1000/1e6·2 + 500/1e6·4) = 2 × (0.002 + 0.002) = 0.008
	want := 2.0 * (1000.0/1e6*2.0 + 500.0/1e6*4.0)
	if meta.Cost < want-1e-9 || meta.Cost > want+1e-9 {
		t.Fatalf("session cost = %v, want ~%v", meta.Cost, want)
	}
}

// No CostPerMillion wired → tokens still tracked, cost stays 0.
func TestUsageNoPricing(t *testing.T) {
	store := makeFauxStore(t)
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m", ContextWindow: 100000},
		provider.FauxTurn{Text: "hi", Usage: bigUsage(10, 5)})
	runner := &core.Runner{Store: store, StreamFn: faux.Stream, Model: faux.Model, MaxTurns: 2}
	events, err := runner.Run(context.Background(), "s1", []*core.Message{core.NewUserMessage("go")})
	if err != nil {
		t.Fatal(err)
	}
	drainWithTimeout(t, events, 5*time.Second)
	meta, _ := store.GetSession(context.Background(), "s1")
	if meta.TokensIn != 10 || meta.TokensOut != 5 || meta.Cost != 0 {
		t.Fatalf("session = in %d out %d cost %v, want 10/5/0", meta.TokensIn, meta.TokensOut, meta.Cost)
	}
}

// A provider that hangs (produces nothing until the per-turn context is
// canceled) must be cut off by TurnTimeout, so the run completes instead of
// blocking forever.
func TestTurnTimeoutCutsHungProvider(t *testing.T) {
	store := makeFauxStore(t)
	hangFn := func(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
		ch := make(chan core.StreamEvent, 1)
		go func() {
			defer close(ch)
			<-ctx.Done() // simulate a stuck provider: wake only on cancellation
		}()
		return ch, nil
	}
	runner := &core.Runner{
		Store: store, StreamFn: hangFn,
		Model:       core.Model{Provider: "faux", ID: "m", ContextWindow: 100000},
		TurnTimeout: 80 * time.Millisecond, MaxTurns: 1,
	}
	events, err := runner.Run(context.Background(), "s1", []*core.Message{core.NewUserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	got := drainWithTimeout(t, events, 5*time.Second)
	var sawEnd bool
	for _, ev := range got {
		if ev.Type == core.AgentEnd {
			sawEnd = true
		}
	}
	if !sawEnd {
		t.Fatal("agent_end not seen after a hung provider was cut off")
	}
}

// hangTool blocks until its context is canceled, then fails with the ctx error.
type hangTool struct{}

func (hangTool) Name() string             { return "hang" }
func (hangTool) Schema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (hangTool) ExecutionMode() core.Mode { return core.ModeParallel }
func (hangTool) Execute(ctx context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	<-ctx.Done()
	return core.ToolResult{}, ctx.Err()
}

// ToolTimeout cuts a hung tool; the run surfaces an error result the model can
// react to, then continues (the loop is not wedged).
func TestToolTimeout(t *testing.T) {
	store := makeFauxStore(t)
	turns := []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "hang"}}},
		{Text: "recovered"},
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m", ContextWindow: 100000}, turns...)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Tools: []core.Tool{hangTool{}},
		Model: faux.Model, MaxTurns: 4, ToolTimeout: 80 * time.Millisecond,
	}
	events, err := runner.Run(context.Background(), "s1", []*core.Message{core.NewUserMessage("go")})
	if err != nil {
		t.Fatal(err)
	}
	got := drainWithTimeout(t, events, 5*time.Second)

	var sawTimeout, sawRecover, sawEnd bool
	for _, ev := range got {
		if ev.Type == core.ToolExecEnd && ev.IsError && ev.Result != nil && strings.Contains(ev.Result.Output, "timed out") {
			sawTimeout = true
		}
		if ev.Type == core.MessageEnd && ev.Message != nil && ev.Message.Text() == "recovered" {
			sawRecover = true
		}
		if ev.Type == core.AgentEnd {
			sawEnd = true
		}
	}
	if !sawTimeout || !sawRecover || !sawEnd {
		t.Fatalf("timeout=%v recover=%v end=%v, want all three", sawTimeout, sawRecover, sawEnd)
	}
}
