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

// newRecoverFixture is like newConfirmTestGateway but also returns the store
// and the gateway (for Shutdown), so phase 4 tests can inspect status and
// drive graceful shutdown / resume.
func newRecoverFixture(t *testing.T, turns []provider.FauxTurn, opts ...Option) (*session.MemoryStore, *Gateway, *httptest.Server, *riskyTool) {
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
	return store, gw, srv, risky
}

// seedEntry appends one message entry to the store (test setup for history).
func seedEntry(t *testing.T, store *session.MemoryStore, sessionID string, typ session.EntryType, m *core.Message) {
	t.Helper()
	payload, _ := json.Marshal(m)
	if _, err := store.AppendEntry(context.Background(), sessionID, typ, payload, nil); err != nil {
		t.Fatal(err)
	}
}

// waitStatus polls the store until the session reaches want (or times out).
func waitStatus(t *testing.T, store *session.MemoryStore, id string, want session.SessionStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		meta, err := store.GetSession(context.Background(), id)
		if err == nil && meta.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session %s to reach status %q", id, want)
}

// TestRunContinuesWithoutClient: the run must progress and persist events
// even when no SSE client is connected (DESIGN §6.5 "断线继续运行"); a later
// reconnect replays the full stream exactly.
func TestRunContinuesWithoutClient(t *testing.T) {
	store, _, srv, _ := newRecoverFixture(t,
		[]provider.FauxTurn{{Text: "hello, running unwatched"}},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	postJSON(t, base+"/prompts", `{"text":"go"}`)
	// No SSE client at all. Wait for the run to finish on its own — the run
	// must progress and persist its events even though nobody is watching.
	waitStatus(t, store, "s1", session.StatusIdle)

	// A fresh client catches up: replay from seq 0 must yield the whole run.
	frames, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	findEvent(t, frames, core.AgentStart)
	// The turn's text (produced while unwatched) must be present in the replay.
	var sawText bool
	for _, f := range frames {
		var ev core.AgentEvent
		if json.Unmarshal([]byte(f), &ev) == nil && ev.Type == core.MessageEnd && ev.Message != nil &&
			ev.Message.Text() == "hello, running unwatched" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("replay missed the unwatched turn")
	}
	findEvent(t, frames, core.AgentEnd)
}

// TestDisconnectReconnect: a client that disconnects mid-run (here the run is
// paused on a confirmation) reconnects and catches up exactly — the events
// produced after it reconnected arrive live, with no gaps or duplicates.
func TestDisconnectReconnect(t *testing.T) {
	store, _, srv, _ := newRecoverFixture(t,
		[]provider.FauxTurn{
			{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
			{Text: "final"},
		},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	postJSON(t, base+"/prompts", `{"text":"do it"}`)
	// SSE #1: connect, read until the run pauses on the confirmation, then
	// disconnect (readSSEUntil closes the connection on return).
	f1, err := readSSEUntil(t, base+"/events?after=0", core.ToolConfirmRequest, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := findEvent(t, f1, core.ToolConfirmRequest)

	// Reconnect (SSE #2) and wait for the full stream. The run is still
	// blocked; it continues once we answer the confirmation below.
	framesCh := make(chan []string, 1)
	errCh := make(chan error, 1)
	go func() {
		f, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		framesCh <- f
	}()
	// Let the reconnected client establish and replay the so-far events.
	time.Sleep(100 * time.Millisecond)

	// Answer the confirmation; the run continues and the live stream picks up
	// the new events even though the original client is gone.
	if code, _ := postJSON(t, base+"/confirmations", `{"id":"`+req.ConfirmationID+`","decision":"allow"}`); code != http.StatusOK {
		t.Fatalf("confirm: %d", code)
	}
	var f2 []string
	select {
	case f2 = <-framesCh:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for reconnected stream")
	}

	// The reconnected stream (replayed from seq 0 + live tail) contains the
	// whole exchange, ending at agent_end.
	findEvent(t, f2, core.ToolConfirmRequest)
	if resp := findEvent(t, f2, core.ToolConfirmResponse); resp.IsError {
		t.Fatalf("response = %+v, want allowed", resp)
	}
	findEvent(t, f2, core.ToolExecEnd)
	findEvent(t, f2, core.AgentEnd)

	// The run completed normally, so the session ends idle (not interrupted).
	waitStatus(t, store, "s1", session.StatusIdle)
}

// TestGracefulShutdown: Shutdown() cancels an in-flight run and records the
// session interrupted (DESIGN §6.5), so a restarted process can resume it.
func TestGracefulShutdown(t *testing.T) {
	store, gw, srv, _ := newRecoverFixture(t,
		[]provider.FauxTurn{
			{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
			{Text: "final"},
		},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	postJSON(t, base+"/prompts", `{"text":"do it"}`)
	// Wait until the run is in-flight and paused on the confirmation.
	if _, err := readSSEUntil(t, base+"/events?after=0", core.ToolConfirmRequest, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, store, "s1", session.StatusRunning)

	// Graceful shutdown cancels the run and marks the session interrupted.
	gw.Shutdown()
	waitStatus(t, store, "s1", session.StatusInterrupted)
}

// TestResume: after a crash left the session interrupted (with a dangling
// tool call in the history), POST /resume restarts the loop from the last
// complete message — repairing the dangling call and running to completion.
func TestResume(t *testing.T) {
	store, _, srv, _ := newRecoverFixture(t,
		[]provider.FauxTurn{{Text: "continuing to completion"}},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	// Simulate a prior crash: a partial history ending in a dangling tool
	// call, and the session marked interrupted.
	seedEntry(t, store, "s1", session.EntryUser, core.NewUserMessage("do it"))
	seedEntry(t, store, "s1", session.EntryAssistant, &core.Message{
		Role: core.RoleAssistant,
		Content: []core.Block{
			core.TextBlock("let me run the risky tool"),
			core.ToolCallBlock("tc9", "risky", nil),
		},
		StopReason: core.StopToolUse,
	})
	meta, _ := store.GetSession(context.Background(), "s1")
	meta.Status = session.StatusInterrupted
	if err := store.UpdateSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}

	code, _ := postJSON(t, base+"/resume", `{}`)
	if code != http.StatusOK {
		t.Fatalf("resume: %d, want 200", code)
	}
	frames, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	findEvent(t, frames, core.AgentEnd)
	waitStatus(t, store, "s1", session.StatusIdle)

	// The dangling tool call got a synthesized error result (context is now
	// valid for the provider).
	entries, _ := store.ListEntries(context.Background(), "s1", 0, 0)
	var repaired bool
	for _, e := range entries {
		if e.Type != session.EntryToolResult {
			continue
		}
		var m core.Message
		if json.Unmarshal(e.Payload, &m) == nil && m.ToolCallID == "tc9" && m.IsError {
			repaired = true
		}
	}
	if !repaired {
		t.Fatalf("dangling tool call tc9 was not repaired on resume")
	}
}

// TestResumeValidation: unknown session → 404; non-interrupted session → 409.
func TestResumeValidation(t *testing.T) {
	_, _, srv, _ := newRecoverFixture(t, []provider.FauxTurn{{Text: "x"}})

	if code, _ := postJSON(t, srv.URL+"/api/agent/sessions/nope/resume", `{}`); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/agent/sessions/s1/resume", `{}`); code != http.StatusConflict {
		t.Fatalf("not interrupted: %d, want 409", code)
	}
}
