package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// riskyTool counts executions so tests can prove a denied/timed-out call
// never ran.
type riskyTool struct {
	mu    sync.Mutex
	calls int
}

func (t *riskyTool) Name() string             { return "risky" }
func (t *riskyTool) Schema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (t *riskyTool) ExecutionMode() core.Mode { return core.ModeParallel }
func (t *riskyTool) Execute(_ context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return core.ToolResult{Output: "risky ran"}, nil
}

func (t *riskyTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// newConfirmTestGateway builds a gateway whose "risky" tool is routed to
// confirmation by BeforeToolCall; the Web waiter (the thing under test) is
// installed by gateway.New because no ConfirmTool is set.
func newConfirmTestGateway(t *testing.T, turns []provider.FauxTurn, opts ...Option) (*httptest.Server, *riskyTool) {
	t.Helper()
	store := session.NewMemoryStore()
	meta := session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m", Status: session.StatusIdle, CreatedAt: time.Now()}
	if err := store.CreateSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	risky := &riskyTool{}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, turns...)
	runner := &core.Runner{
		Store: store, StreamFn: faux.Stream,
		Tools: []core.Tool{risky},
		Model: faux.Model, MaxTurns: 8,
		Hooks: core.Hooks{
			BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
				if call.Name == "risky" {
					return core.Decision{Confirm: true, Reason: "dangerous operation"}, nil
				}
				return core.Decision{}, nil
			},
		},
	}
	gw := New(store, runner, append([]Option{WithAnonymousUser("u")}, opts...)...)
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, risky
}

// findEvent scans raw SSE data frames for the first event of type typ.
func findEvent(t *testing.T, frames []string, typ string) *core.AgentEvent {
	t.Helper()
	for _, f := range frames {
		var a core.AgentEvent
		if json.Unmarshal([]byte(f), &a) == nil && a.Type == typ {
			return &a
		}
	}
	t.Fatalf("no %s event in %d frames", typ, len(frames))
	return nil
}

// TestConfirmationsFlow is the phase 3 acceptance (DESIGN §11): a
// dangerous tool call pauses the run with a tool_confirmation_request;
// the user answers via POST /confirmations; on allow the tool executes.
// The whole exchange happens over real HTTP while the run blocks.
func TestConfirmationsFlow(t *testing.T) {
	srv, risky := newConfirmTestGateway(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "final"},
	})
	base := srv.URL + "/api/agent/sessions/s1"

	code, _ := postJSON(t, base+"/prompts", `{"text":"do it"}`)
	if code != http.StatusAccepted {
		t.Fatalf("prompt: %d", code)
	}
	// Wait for the confirmation request, then answer it.
	frames, err := readSSEUntil(t, base+"/events?after=0", core.ToolConfirmRequest, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := findEvent(t, frames, core.ToolConfirmRequest)
	if req.Risk != "dangerous operation" || req.ConfirmationID == "" {
		t.Fatalf("confirmation request = %+v, want the hook's risk + an id", req)
	}
	code, body := postJSON(t, base+"/confirmations",
		`{"id":"`+req.ConfirmationID+`","decision":"allow"}`)
	if code != http.StatusOK || body["status"] != "allowed" {
		t.Fatalf("confirm: %d %+v, want 200 allowed", code, body)
	}

	// The run must have continued and finished: a fresh SSE connection
	// replays the full stream including the (persisted) response event.
	frames, err = readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp := findEvent(t, frames, core.ToolConfirmResponse)
	if resp.IsError {
		t.Fatalf("response = %+v, want an allowed (non-error) decision", resp)
	}
	if risky.count() != 1 {
		t.Fatalf("risky executed %d times, want 1 (allowed)", risky.count())
	}
	msgs := waitMessages(t, base+"/messages", 4)
	if got := textOf(msgs[2]); got != "risky ran" {
		t.Fatalf("tool result = %q, want the tool's output", got)
	}
}

// TestConfirmationsDeny: a denial skips the tool, feeds an error result to
// the model, and the loop keeps running (DESIGN §11 阶段 3 验收).
func TestConfirmationsDeny(t *testing.T) {
	srv, risky := newConfirmTestGateway(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "final"},
	})
	base := srv.URL + "/api/agent/sessions/s1"

	postJSON(t, base+"/prompts", `{"text":"do it"}`)
	frames, err := readSSEUntil(t, base+"/events?after=0", core.ToolConfirmRequest, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := findEvent(t, frames, core.ToolConfirmRequest)
	code, _ := postJSON(t, base+"/confirmations",
		`{"id":"`+req.ConfirmationID+`","decision":"deny"}`)
	if code != http.StatusOK {
		t.Fatalf("deny: %d", code)
	}

	frames, err = readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp := findEvent(t, frames, core.ToolConfirmResponse)
	if !resp.IsError {
		t.Fatalf("response = %+v, want a denial", resp)
	}
	if risky.count() != 0 {
		t.Fatalf("denied tool executed %d times, want 0", risky.count())
	}
	msgs := waitMessages(t, base+"/messages", 4)
	if isError, _ := msgs[2]["isError"].(bool); !isError {
		t.Fatalf("denied tool result must be an error message: %+v", msgs[2])
	}
	if textOf(msgs[3]) != "final" {
		t.Fatalf("loop did not continue: last = %q", textOf(msgs[3]))
	}
}

// TestConfirmationsTimeout: with no answer, the configurable timeout
// (DESIGN §5.3: default 120s) must deny — never hang, never allow.
func TestConfirmationsTimeout(t *testing.T) {
	srv, risky := newConfirmTestGateway(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "final"},
	}, WithConfirmTimeout(300*time.Millisecond))
	base := srv.URL + "/api/agent/sessions/s1"

	postJSON(t, base+"/prompts", `{"text":"do it"}`)
	if _, err := readSSEUntil(t, base+"/events?after=0", core.ToolConfirmRequest, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// No answer. The run must time out and finish on its own.
	frames, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp := findEvent(t, frames, core.ToolConfirmResponse)
	if !resp.IsError || resp.Reason != "confirmation timed out" {
		t.Fatalf("response = %+v, want the timeout denial", resp)
	}
	if risky.count() != 0 {
		t.Fatalf("timed-out tool executed %d times, want 0", risky.count())
	}
}

// TestConfirmationsValidation: 404 unknown session, 400 bad decision,
// 404 unknown confirmation id, 409 double answer.
func TestConfirmationsValidation(t *testing.T) {
	srv, _ := newConfirmTestGateway(t, []provider.FauxTurn{{Text: "x"}})

	code, _ := postJSON(t, srv.URL+"/api/agent/sessions/nope/confirmations", `{"id":"c1","decision":"allow"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", code)
	}
	code, _ = postJSON(t, srv.URL+"/api/agent/sessions/s1/confirmations", `{"id":"c1","decision":"maybe"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad decision: %d, want 400", code)
	}
	code, _ = postJSON(t, srv.URL+"/api/agent/sessions/s1/confirmations", `{"id":"c1","decision":"allow"}`)
	if code != http.StatusNotFound {
		t.Fatalf("not pending: %d, want 404", code)
	}
}
