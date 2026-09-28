package core

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/session"
)

// fakeSessionTool implements SessionTool: it records which path the loop
// routed to and the session id it saw. Execute flags the "wrong path" so a
// dispatch regression is visible.
type fakeSessionTool struct {
	name            string
	output          string
	executeInCalled bool
	executeCalled   bool
	gotSession      string
}

func (f *fakeSessionTool) Name() string            { return f.name }
func (f *fakeSessionTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *fakeSessionTool) ExecutionMode() Mode     { return ModeParallel }
func (f *fakeSessionTool) Execute(context.Context, ToolCall, func(ToolUpdate)) (ToolResult, error) {
	f.executeCalled = true
	return ToolResult{Output: "execute-path-wrong", IsError: true}, nil
}
func (f *fakeSessionTool) ExecuteIn(_ context.Context, sessionID string, _ ToolCall, _ func(ToolUpdate)) (ToolResult, error) {
	f.executeInCalled = true
	f.gotSession = sessionID
	return ToolResult{Output: f.output}, nil
}

// fakePlainTool implements only Tool (no ExecuteIn) — the non-breaking path.
type fakePlainTool struct {
	name     string
	executed bool
}

func (f *fakePlainTool) Name() string            { return f.name }
func (f *fakePlainTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *fakePlainTool) ExecutionMode() Mode     { return ModeParallel }
func (f *fakePlainTool) Execute(context.Context, ToolCall, func(ToolUpdate)) (ToolResult, error) {
	f.executed = true
	return ToolResult{Output: "ok"}, nil
}

// TestRunOneToolDispatchesSessionTool proves the non-breaking dispatch: a
// SessionTool is routed to ExecuteIn with the session id, while a plain Tool
// keeps the original Execute path untouched.
func TestRunOneToolDispatchesSessionTool(t *testing.T) {
	st := &fakeSessionTool{name: "sess", output: "from-executein"}
	plain := &fakePlainTool{name: "plain"}
	r := &Runner{Tools: []Tool{st, plain}}
	ctx := context.Background()
	noop := func(AgentEvent) {}

	res := r.runOneTool(ctx, "s42", ToolCall{Name: "sess", Arguments: json.RawMessage(`{}`)}, noop)
	if !st.executeInCalled || st.executeCalled {
		t.Fatalf("session tool routing wrong: executeIn=%v execute=%v (want executeIn only)", st.executeInCalled, st.executeCalled)
	}
	if st.gotSession != "s42" {
		t.Fatalf("ExecuteIn got session %q, want s42", st.gotSession)
	}
	if res.IsError || res.Output != "from-executein" {
		t.Fatalf("session tool result = %+v, want from-executein", res)
	}

	if res := r.runOneTool(ctx, "s42", ToolCall{Name: "plain"}, noop); res.IsError {
		t.Fatalf("plain tool: unexpected error %+v", res)
	}
	if !plain.executed {
		t.Fatal("plain tool's Execute was not called (a plain tool was wrongly routed to ExecuteIn)")
	}
}

// seqFromPlaceholder extracts the seq from "[结果已归档,seq=N,可用 recall_event 取回]".
func seqFromPlaceholder(s string) int64 {
	i := strings.Index(s, "seq=")
	if i < 0 {
		return -1
	}
	rest := s[i+len("seq="):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	n, _ := strconv.ParseInt(rest[:j], 10, 64)
	return n
}

// TestRecallEventRoundTrip is the L1→recall round trip: an old tool result is
// evicted to a placeholder by buildContext (L1), the placeholder carries the
// store seq, and recall_event{seq} reads the original back — with the original
// entry still intact in the append-only store.
func TestRecallEventRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	if err := store.CreateSession(ctx, session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	appendUser := func(text string) {
		payload, _ := json.Marshal(NewUserMessage(text))
		if _, err := store.AppendEntry(ctx, "s1", session.EntryUser, payload, nil); err != nil {
			t.Fatal(err)
		}
	}
	// One early tool result, then enough later user turns that L1
	// (EvictOlderThan=2) archives it.
	appendUser("q1")
	toolMsg := &Message{Role: RoleTool, ToolCallID: "t1", ToolName: "echo", Content: []Block{TextBlock("ARCHIVED-ORIGINAL")}}
	toolPayload, _ := json.Marshal(toolMsg)
	toolSeq, err := store.AppendEntry(ctx, "s1", session.EntryToolResult, toolPayload, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"q2", "q3", "q4", "q5"} {
		appendUser(q)
	}

	r := &Runner{Store: store, Compaction: CompactionConfig{Enabled: true, EvictOlderThan: 2}}
	_, msgs, _ := r.buildContext(ctx, "s1")

	var placeholder string
	for _, m := range msgs {
		if m.Role == RoleTool && strings.Contains(m.Text(), "结果已归档") {
			placeholder = m.Text()
		}
	}
	if placeholder == "" {
		t.Fatal("L1 did not archive the old tool result — no placeholder in the derived context")
	}
	if got := seqFromPlaceholder(placeholder); got != toolSeq {
		t.Fatalf("placeholder seq = %d, want store seq %d", got, toolSeq)
	}

	res, err := RecallEventTool{Store: store}.ExecuteIn(ctx, "s1",
		ToolCall{Arguments: json.RawMessage(`{"seq":` + strconv.FormatInt(toolSeq, 10) + `}`)},
		func(ToolUpdate) {})
	if err != nil || res.IsError {
		t.Fatalf("recall_event: err=%v result=%+v", err, res)
	}
	if res.Output != "ARCHIVED-ORIGINAL" {
		t.Fatalf("recall_event output = %q, want ARCHIVED-ORIGINAL", res.Output)
	}
}

// TestRecallEventErrors: bad/missing seq and a nil store are IsError results
// the model can react to — never a loop-breaking error.
func TestRecallEventErrors(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	_ = store.CreateSession(ctx, session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m"})
	noop := func(ToolUpdate) {}

	cases := []struct {
		name  string
		store session.Store
		args  string
	}{
		{"bad args", store, `not-json`},
		{"zero seq", store, `{"seq":0}`},
		{"missing entry", store, `{"seq":9999}`},
		{"nil store", nil, `{"seq":1}`},
	}
	for _, c := range cases {
		res, err := RecallEventTool{Store: c.store}.ExecuteIn(ctx, "s1", ToolCall{Arguments: json.RawMessage(c.args)}, noop)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		if !res.IsError {
			t.Fatalf("%s: want IsError result, got %+v", c.name, res)
		}
	}
}
