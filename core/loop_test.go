package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// echoTool is a trivial tool proving the tool-call round trip.
type echoTool struct{}

func (echoTool) Name() string { return "echo" }

func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (echoTool) ExecutionMode() core.Mode { return core.ModeSequential }

func (echoTool) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	var a struct{ Text string }
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return core.ToolResult{Output: "bad args", IsError: true}, nil
	}
	return core.ToolResult{Output: "echo: " + a.Text}, nil
}

// runScript drives a full run against the faux provider and returns the
// event stream plus the store it was persisted into.
func runScript(t *testing.T, turns []provider.FauxTurn, hooks core.Hooks) ([]core.AgentEvent, *session.MemoryStore) {
	t.Helper()
	ctx := context.Background()
	store := session.NewMemoryStore()
	if err := store.CreateSession(ctx, session.SessionMeta{
		ID: "s1", UserID: "u1", Provider: "faux", Model: "faux-mini",
	}); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(
		core.Model{Provider: "faux", ID: "faux-mini", ContextWindow: 128000},
		turns...,
	)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Tools: []core.Tool{echoTool{}},
		Model: faux.Model, MaxTurns: 8, Hooks: hooks,
	}
	events, err := runner.Run(ctx, "s1", []*core.Message{core.NewUserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	var got []core.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	return got, store
}

func TestRunLoopHappyPath(t *testing.T) {
	got, store := runScript(t, []provider.FauxTurn{
		{
			Text:      "let me echo",
			ToolCalls: []provider.FauxToolCall{{Name: "echo", Args: json.RawMessage(`{"text":"hi"}`)}},
		},
		{Text: "done"},
	}, core.Hooks{})
	if len(got) == 0 {
		t.Fatal("no events")
	}
	if got[0].Type != core.AgentStart {
		t.Fatalf("first event = %s, want agent_start", got[0].Type)
	}
	last := got[len(got)-1]
	if last.Type != core.AgentEnd || len(last.Messages) != 4 {
		t.Fatalf("agent_end = {type:%s msgs:%d}, want 4 messages (user, assistant, tool, assistant)",
			last.Type, len(last.Messages))
	}

	var sawExecStart, sawExecEnd, sawDelta bool
	for _, ev := range got {
		switch ev.Type {
		case core.ToolExecStart:
			sawExecStart = ev.ToolName == "echo"
		case core.ToolExecEnd:
			sawExecEnd = ev.Result != nil && ev.Result.Output == "echo: hi"
		case core.TextDelta:
			sawDelta = true
		}
	}
	if !sawExecStart || !sawExecEnd || !sawDelta {
		t.Fatalf("missing events: execStart=%v execEnd=%v delta=%v", sawExecStart, sawExecEnd, sawDelta)
	}

	// Store must hold exactly: user, assistant, tool_result, assistant.
	entries, err := store.ListEntries(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []session.EntryType
	for _, e := range entries {
		kinds = append(kinds, e.Type)
	}
	want := []session.EntryType{
		session.EntryUser, session.EntryAssistant, session.EntryToolResult, session.EntryAssistant,
	}
	if len(kinds) != len(want) {
		t.Fatalf("entry kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("entry %d = %s, want %s", i, kinds[i], want[i])
		}
	}
}

func TestBeforeToolCallBlock(t *testing.T) {
	got, _ := runScript(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "echo", Args: json.RawMessage(`{"text":"x"}`)}}},
		{Text: "ok"},
	}, core.Hooks{
		BeforeToolCall: func(ctx context.Context, call core.ToolCall) (core.Decision, error) {
			return core.Decision{Block: true, Reason: "denied by policy"}, nil
		},
	})
	blocked := false
	for _, ev := range got {
		if ev.Type == core.ToolExecEnd && ev.IsError && ev.Result != nil &&
			ev.Result.Output == "denied by policy" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("expected the blocked tool to surface an error result")
	}
	// The loop must keep running after a block: the second (text-only)
	// turn still happened.
	if got[len(got)-1].Type != core.AgentEnd {
		t.Fatalf("last event = %s, want agent_end", got[len(got)-1].Type)
	}
}

func TestMissingToolIsErrorResultNotCrash(t *testing.T) {
	got, _ := runScript(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "nope", Args: json.RawMessage(`{}`)}}},
		{Text: "ok"},
	}, core.Hooks{})
	notFound := false
	for _, ev := range got {
		if ev.Type == core.ToolExecEnd && ev.IsError && ev.Result != nil &&
			ev.Result.Output == "Tool nope not found" {
			notFound = true
		}
	}
	if !notFound {
		t.Fatal("expected a 'tool not found' error result fed back to the model")
	}
}

// TestRunnerSamplingCopiedOntoRequest: the integrator fills Runner.Sampling
// once; every turn's StreamFn must see the same knobs. Zero Sampling is the
// default (covered by every other test in this file).
func TestRunnerSamplingCopiedOntoRequest(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	if err := store.CreateSession(ctx, session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	var seen []core.Request
	inner := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, provider.FauxTurn{Text: "ok"}).Stream
	fn := func(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
		seen = append(seen, req)
		return inner(ctx, req)
	}
	want := core.Sampling{Temperature: core.Ptr(0.3), MaxTokens: 128}
	runner := &core.Runner{
		Store: store, StreamFn: fn, Model: core.Model{Provider: "faux", ID: "m"},
		Sampling: want, MaxTurns: 1,
	}
	events, err := runner.Run(ctx, "s1", []*core.Message{core.NewUserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(seen) != 1 {
		t.Fatalf("StreamFn called %d times, want 1", len(seen))
	}
	got := seen[0].Sampling
	if got.Temperature == nil || *got.Temperature != 0.3 || got.MaxTokens != 128 {
		t.Fatalf("Sampling = %+v, want temperature 0.3 max_tokens 128", got)
	}
	if got.TopP != nil || got.Seed != nil {
		t.Fatalf("unset knobs must stay nil: %+v", got)
	}
}

func TestPromptIDCopiedOntoAssistantAndTool(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	if err := store.CreateSession(ctx, session.SessionMeta{
		ID: "s1", UserID: "u1", Provider: "faux", Model: "faux-mini",
	}); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(
		core.Model{Provider: "faux", ID: "faux-mini", ContextWindow: 128000},
		provider.FauxTurn{
			Text:      "let me echo",
			ToolCalls: []provider.FauxToolCall{{Name: "echo", Args: json.RawMessage(`{"text":"hi"}`)}},
		},
		provider.FauxTurn{Text: "done"},
	)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream, Tools: []core.Tool{echoTool{}},
		Model: faux.Model, MaxTurns: 8,
	}
	user := core.NewUserMessage("hello")
	user.PromptID = "p_test"
	events, err := runner.Run(ctx, "s1", []*core.Message{user})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	entries, err := store.ListEntries(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawAssistant, sawTool bool
	for _, e := range entries {
		if e.Type != session.EntryAssistant && e.Type != session.EntryToolResult {
			continue
		}
		var m core.Message
		if json.Unmarshal(e.Payload, &m) != nil {
			t.Fatalf("unmarshal %s: %s", e.Type, e.Payload)
		}
		if m.PromptID != "p_test" {
			t.Fatalf("%s promptId = %q, want p_test", e.Type, m.PromptID)
		}
		if e.Type == session.EntryAssistant {
			sawAssistant = true
		} else {
			sawTool = true
		}
	}
	if !sawAssistant || !sawTool {
		t.Fatalf("assistant=%v tool=%v, want both stamped", sawAssistant, sawTool)
	}
}
