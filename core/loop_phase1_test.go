package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// funcTool is a tool defined by a closure.
type funcTool struct {
	name string
	mode core.Mode
	exec func(ctx context.Context, call core.ToolCall, update func(core.ToolUpdate)) core.ToolResult
}

func (f *funcTool) Name() string             { return f.name }
func (f *funcTool) Schema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (f *funcTool) ExecutionMode() core.Mode { return f.mode }
func (f *funcTool) Execute(ctx context.Context, call core.ToolCall, update func(core.ToolUpdate)) (core.ToolResult, error) {
	return f.exec(ctx, call, update), nil
}

// countingTool records executions so tests can prove a call did NOT run.
type countingTool struct {
	mu    sync.Mutex
	name  string
	mode  core.Mode
	calls int
}

func (c *countingTool) Name() string             { return c.name }
func (c *countingTool) Schema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (c *countingTool) ExecutionMode() core.Mode { return c.mode }
func (c *countingTool) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return core.ToolResult{Output: "ran:" + call.ID}, nil
}

// TestTruncatedToolCallsAreNotExecuted: a StopLength assistant message with
// tool calls must fail the whole batch without executing anything (DESIGN
// §2.3), and the loop must keep going so the model can re-issue.
func TestTruncatedToolCallsAreNotExecuted(t *testing.T) {
	echo := &countingTool{name: "echo", mode: core.ModeParallel}
	got, store := runScriptWithTools(t, []provider.FauxTurn{
		{
			StopReason: core.StopLength,
			ToolCalls:  []provider.FauxToolCall{{Name: "echo", Args: json.RawMessage(`{"text":"hi"}`)}},
		},
		{Text: "done"},
	}, core.Hooks{}, echo)

	echo.mu.Lock()
	ran := echo.calls
	echo.mu.Unlock()
	if ran != 0 {
		t.Fatalf("truncated tool call was executed %d times, want 0", ran)
	}

	failed, sawExecStart := false, false
	for _, ev := range got {
		switch ev.Type {
		case core.ToolExecStart:
			sawExecStart = true
		case core.ToolExecEnd:
			if ev.IsError && ev.Result != nil && strings.Contains(ev.Result.Output, "output token limit") {
				failed = true
			}
		}
	}
	if !sawExecStart || !failed {
		t.Fatalf("expected start + truncation error result, sawStart=%v sawFail=%v", sawExecStart, failed)
	}
	// The loop kept running: the store holds user, assistant, tool_result, assistant.
	entries, err := store.ListEntries(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 || entries[2].Type != session.EntryToolResult {
		t.Fatalf("entries = %d, want 4 with tool_result third", len(entries))
	}
}

// TestParallelToolExecution: parallel-mode calls overlap in time, yet the
// store receives tool-result entries in the original call order.
func TestParallelToolExecution(t *testing.T) {
	var mu sync.Mutex
	idx := map[string]int{"a": 0, "b": 1, "c": 2}
	var starts, ends [3]time.Time
	slow := &funcTool{
		name: "slow",
		mode: core.ModeParallel,
		exec: func(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) core.ToolResult {
			var a struct {
				Which string `json:"which"`
			}
			_ = json.Unmarshal(call.Arguments, &a)
			mu.Lock()
			i := idx[a.Which]
			if starts[i].IsZero() {
				starts[i] = time.Now()
			}
			mu.Unlock()
			time.Sleep(map[string]time.Duration{"a": 120 * time.Millisecond, "b": 10 * time.Millisecond, "c": 60 * time.Millisecond}[a.Which])
			mu.Lock()
			ends[i] = time.Now()
			mu.Unlock()
			return core.ToolResult{Output: "slow:" + a.Which}
		},
	}

	got, store := runScriptWithTools(t, []provider.FauxTurn{
		{
			ToolCalls: []provider.FauxToolCall{
				{Name: "slow", Args: json.RawMessage(`{"which":"a"}`)},
				{Name: "slow", Args: json.RawMessage(`{"which":"b"}`)},
				{Name: "slow", Args: json.RawMessage(`{"which":"c"}`)},
			},
		},
		{Text: "done"},
	}, core.Hooks{}, slow)

	endCount := 0
	for _, ev := range got {
		if ev.Type == core.ToolExecEnd {
			endCount++
		}
	}
	if endCount != 3 {
		t.Fatalf("tool_execution_end events = %d, want 3", endCount)
	}

	mu.Lock()
	// Parallel: c (60ms) started before a (120ms) finished.
	overlap := starts[2].Before(ends[0])
	wall := time.Duration(0)
	first, last := starts[0], ends[0]
	for i := 0; i < 3; i++ {
		if starts[i].Before(first) {
			first = starts[i]
		}
		if ends[i].After(last) {
			last = ends[i]
		}
	}
	wall = last.Sub(first)
	mu.Unlock()
	if !overlap {
		t.Fatalf("calls did not overlap in time: a=[%v,%v] c starts %v", starts[0], ends[0], starts[2])
	}
	if wall >= 180*time.Millisecond {
		t.Fatalf("apparent serial execution: batch wall time %v (a+b+c = 190ms serial)", wall)
	}

	// Store order must be the original call order (a, b, c), even though b
	// finished first. Faux ids are faux_0_<callIndex>.
	entries, err := store.ListEntries(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range entries {
		if e.Type != session.EntryToolResult {
			continue
		}
		var m core.Message
		if json.Unmarshal(e.Payload, &m) != nil {
			t.Fatal("unmarshal tool entry")
		}
		switch m.ToolCallID {
		case "faux_0_0":
			order = append(order, "a")
		case "faux_0_1":
			order = append(order, "b")
		case "faux_0_2":
			order = append(order, "c")
		}
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("store tool-result order = %v, want [a b c]", order)
	}
}

// TestSequentialToolDowngradesBatch: one sequential tool in a batch forces
// the whole batch to run in order (pi semantics).
func TestSequentialToolDowngradesBatch(t *testing.T) {
	var mu sync.Mutex
	var seqEnd, parStart time.Time
	seq := &funcTool{
		name: "seq",
		mode: core.ModeSequential,
		exec: func(_ context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) core.ToolResult {
			time.Sleep(80 * time.Millisecond)
			mu.Lock()
			seqEnd = time.Now()
			mu.Unlock()
			return core.ToolResult{Output: "seq done"}
		},
	}
	par := &funcTool{
		name: "par",
		mode: core.ModeParallel,
		exec: func(_ context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) core.ToolResult {
			mu.Lock()
			parStart = time.Now()
			mu.Unlock()
			return core.ToolResult{Output: "par done"}
		},
	}

	runScriptWithTools(t, []provider.FauxTurn{
		{
			ToolCalls: []provider.FauxToolCall{
				{Name: "seq", Args: json.RawMessage(`{}`)},
				{Name: "par", Args: json.RawMessage(`{}`)},
			},
		},
		{Text: "done"},
	}, core.Hooks{}, seq, par)

	mu.Lock()
	defer mu.Unlock()
	if seqEnd.IsZero() || parStart.IsZero() {
		t.Fatalf("timeline missing: seqEnd=%v parStart=%v", seqEnd, parStart)
	}
	if parStart.Before(seqEnd) {
		t.Fatalf("parallel tool started before sequential tool finished (batch not downgraded to sequential)")
	}
}

// runScriptWithTools is runScript with a custom tool set.
func runScriptWithTools(t *testing.T, turns []provider.FauxTurn, hooks core.Hooks, tools ...core.Tool) ([]core.AgentEvent, *session.MemoryStore) {
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
		Store: store, StreamFn: faux.Stream, Tools: tools,
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
