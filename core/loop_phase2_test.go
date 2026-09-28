package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// recordingFn wraps a StreamFn and captures the requests it receives, so
// tests can assert what history the next provider actually saw.
type recordingFn struct {
	inner core.StreamFn
	seen  *[]core.Request
}

func (r *recordingFn) Stream(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
	*r.seen = append(*r.seen, req)
	return r.inner(ctx, req)
}

func newPhase2Session(t *testing.T, providerName, model string) *session.MemoryStore {
	t.Helper()
	store := session.NewMemoryStore()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "u1", Provider: providerName, Model: model,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func runWithProviders(t *testing.T, runner *core.Runner) []core.AgentEvent {
	t.Helper()
	events, err := runner.Run(context.Background(), "s1", []*core.Message{core.NewUserMessage("go")})
	if err != nil {
		t.Fatal(err)
	}
	var got []core.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

// TestMidRunModelSwitch is the phase 2 acceptance at core level: the
// session starts on provider A; a tool flips the session's meta to provider
// B mid-run; the switch must take effect at the next turn boundary with a
// model_changed event, and provider B must receive the full converted
// history (DESIGN §3.5: Claude → DeepSeek keeps working).
func TestMidRunModelSwitch(t *testing.T) {
	store := newPhase2Session(t, "claude", "claude-opus")

	// Provider A: answers turn 1 with a tool call that performs the switch.
	providerA := provider.NewFaux(
		core.Model{Provider: "claude", ID: "claude-opus", ContextWindow: 200000},
		provider.FauxTurn{
			Text:      "Switching model for you.",
			ToolCalls: []provider.FauxToolCall{{Name: "switch_model", Args: json.RawMessage(`{}`)}},
		},
	)
	// Provider B: answers whatever comes next.
	providerB := provider.NewFaux(
		core.Model{Provider: "deepseek", ID: "deepseek-chat", ContextWindow: 64000},
		provider.FauxTurn{Text: "from deepseek"},
	)
	var bSeen []core.Request
	bFn := &recordingFn{inner: providerB.Stream, seen: &bSeen}

	var aSeen []core.Request
	aFn := &recordingFn{inner: providerA.Stream, seen: &aSeen}

	switchTool := &funcTool{
		name: "switch_model",
		mode: core.ModeParallel,
		exec: func(ctx context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) core.ToolResult {
			meta, err := store.GetSession(ctx, "s1")
			if err != nil {
				return core.ToolResult{Output: err.Error(), IsError: true}
			}
			meta.Provider = "deepseek"
			meta.Model = "deepseek-chat"
			if err := store.UpdateSession(ctx, meta); err != nil {
				return core.ToolResult{Output: err.Error(), IsError: true}
			}
			return core.ToolResult{Output: "switched to deepseek"}
		},
	}

	runner := &core.Runner{
		Store:    store,
		StreamFn: providerA.Stream, // default; must NOT be used for s1
		Model:    providerA.Model,
		Tools:    []core.Tool{switchTool},
		MaxTurns: 8,
		Providers: map[string]core.ProviderBinding{
			"claude":   {StreamFn: aFn.Stream, Model: core.Model{Provider: "claude", ID: "claude-opus", ContextWindow: 200000}},
			"deepseek": {StreamFn: bFn.Stream, Model: core.Model{Provider: "deepseek", ID: "deepseek-chat", ContextWindow: 64000}},
		},
	}
	got := runWithProviders(t, runner)

	if len(aSeen) != 1 {
		t.Fatalf("provider A calls = %d, want 1", len(aSeen))
	}
	if len(bSeen) != 1 {
		t.Fatalf("provider B calls = %d, want 1 (switch must route turn 2 to B)", len(bSeen))
	}

	// model_changed must fire between turn 1's end and turn 2's start.
	var changed *core.Model
	var sawTurn1End, sawTurn2Start bool
	for i := range got {
		switch got[i].Type {
		case core.TurnEnd:
			if got[i].Turn == 1 {
				sawTurn1End = true
			}
		case core.ModelChanged:
			if !sawTurn1End {
				t.Fatalf("model_changed at %d before turn 1 ended", i)
			}
			if sawTurn2Start {
				t.Fatalf("model_changed at %d after turn 2 started", i)
			}
			changed = got[i].Model
		case core.TurnStart:
			if got[i].Turn == 2 {
				sawTurn2Start = true
			}
		}
	}
	if changed == nil || changed.Provider != "deepseek" || changed.ID != "deepseek-chat" {
		t.Fatalf("model_changed = %+v, want deepseek/deepseek-chat", changed)
	}

	// Provider B must have seen the converted history: user, assistant
	// (with the tool call), tool result.
	req := bSeen[0]
	if req.Model.Provider != "deepseek" || req.Model.ID != "deepseek-chat" {
		t.Fatalf("B request model = %s, want deepseek/deepseek-chat", req.Model.String())
	}
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		roles = append(roles, string(m.Role))
	}
	if len(roles) != 3 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "tool" {
		t.Fatalf("history seen by B = %v, want [user assistant tool]", roles)
	}
}

// TestUnknownSessionProviderFallsBackToDefault: a session whose provider
// has no registered binding must keep running on the runner default (the
// loop warns, never fails the session).
func TestUnknownSessionProviderFallsBackToDefault(t *testing.T) {
	store := newPhase2Session(t, "ghost", "ghost-1")
	def := provider.NewFaux(
		core.Model{Provider: "default", ID: "default-1", ContextWindow: 128000},
		provider.FauxTurn{Text: "default answered"},
	)
	var defSeen []core.Request
	defFn := &recordingFn{inner: def.Stream, seen: &defSeen}

	runner := &core.Runner{
		Store:    store,
		StreamFn: defFn.Stream,
		Model:    def.Model,
		MaxTurns: 8,
		Providers: map[string]core.ProviderBinding{
			"claude": {StreamFn: def.Stream, Model: def.Model},
		},
	}
	got := runWithProviders(t, runner)

	if len(defSeen) != 1 {
		t.Fatalf("default calls = %d, want 1", len(defSeen))
	}
	var finalText string
	for _, ev := range got {
		if ev.Type == core.MessageEnd && ev.Message != nil && ev.Message.Role == core.RoleAssistant {
			finalText = ev.Message.Text()
		}
	}
	if finalText != "default answered" {
		t.Fatalf("final = %q, want the default provider's answer", finalText)
	}
}

// TestHookModelWinsOverSessionModel: an explicit PrepareNextTurn model
// outranks the session-meta model (business rules beat the stored header).
func TestHookModelWinsOverSessionModel(t *testing.T) {
	store := newPhase2Session(t, "claude", "claude-opus")

	providerA := provider.NewFaux(
		core.Model{Provider: "claude", ID: "claude-opus", ContextWindow: 200000},
		provider.FauxTurn{
			ToolCalls: []provider.FauxToolCall{{Name: "noop", Args: json.RawMessage(`{}`)}},
		},
	)
	hookProv := provider.NewFaux(
		core.Model{Provider: "hookprov", ID: "hook-1", ContextWindow: 32000},
		provider.FauxTurn{Text: "hook picked me"},
	)
	var hookSeen []core.Request
	hookFn := &recordingFn{inner: hookProv.Stream, seen: &hookSeen}

	noop := &funcTool{name: "noop", mode: core.ModeParallel,
		exec: func(context.Context, core.ToolCall, func(core.ToolUpdate)) core.ToolResult {
			return core.ToolResult{Output: "ok"}
		}}

	runner := &core.Runner{
		Store:    store,
		StreamFn: providerA.Stream,
		Model:    providerA.Model,
		Tools:    []core.Tool{noop},
		MaxTurns: 8,
		Providers: map[string]core.ProviderBinding{
			"claude":   {StreamFn: providerA.Stream, Model: core.Model{Provider: "claude", ID: "claude-opus", ContextWindow: 200000}},
			"hookprov": {StreamFn: hookFn.Stream, Model: core.Model{Provider: "hookprov", ID: "hook-1", ContextWindow: 32000}},
		},
		Hooks: core.Hooks{
			PrepareNextTurn: func(_ context.Context, _ core.TurnSnapshot) (core.NextTurnConfig, error) {
				m := core.Model{Provider: "hookprov", ID: "hook-1", ContextWindow: 32000}
				return core.NextTurnConfig{Model: &m}, nil
			},
		},
	}
	got := runWithProviders(t, runner)

	if len(hookSeen) != 1 {
		t.Fatalf("hook provider calls = %d, want 1 (hook model must win turn 2)", len(hookSeen))
	}
	var changed *core.Model
	for i := range got {
		if got[i].Type == core.ModelChanged {
			changed = got[i].Model
		}
	}
	if changed == nil || changed.Provider != "hookprov" {
		t.Fatalf("model_changed = %+v, want the hook's model", changed)
	}
	// And the session meta must be untouched by the hook's choice.
	meta, _ := store.GetSession(context.Background(), "s1")
	if meta.Provider != "claude" || meta.Model != "claude-opus" {
		t.Fatalf("session meta = %s/%s, hook must not rewrite it", meta.Provider, meta.Model)
	}
}
