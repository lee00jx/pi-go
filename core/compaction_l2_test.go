package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/session"
)

// bigUser seeds a user message of n ASCII chars (n/4 tokens).
func bigUser(n int) *Message { return NewUserMessage(strings.Repeat("a", n)) }

// runMaybeCompact runs one maybeCompact and returns the emitted events.
func runMaybeCompact(t *testing.T, r *Runner, sid string, model Model) []AgentEvent {
	t.Helper()
	var events []AgentEvent
	r.maybeCompact(context.Background(), sid, model, func(ev AgentEvent) { events = append(events, ev) })
	return events
}

func countCompactions(t *testing.T, st session.Store, sid string) int {
	t.Helper()
	entries, _ := st.ListEntries(context.Background(), sid, 0, 0)
	n := 0
	for _, e := range entries {
		if e.Type == session.EntryCompaction {
			n++
		}
	}
	return n
}

// A context over the window headroom triggers an L2 compaction: a
// compaction entry is appended, the derived context shrinks to summary + a
// small kept tail, and the events fire.
func TestL2CompactionTriggered(t *testing.T) {
	st, sid := seedStore(t)
	// 8 user msgs × 600 ASCII (150 tokens) = 1200 tokens.
	for i := 0; i < 8; i++ {
		appendMsgEntry(t, st, sid, bigUser(600)) // seq 1..8
	}
	r := &Runner{
		Store: st,
		Compaction: CompactionConfig{
			Enabled: true, ReserveTokens: 100, KeepRecentTokens: 100,
			MinMessages: 2, Summarize: func(context.Context, Model, string) (string, error) {
				return "SUMMARY-OK", nil
			},
		},
	}
	model := Model{Provider: "p", ID: "m", ContextWindow: 500} // budget = 400

	if countCompactions(t, st, sid) != 0 {
		t.Fatal("no compaction expected before")
	}
	events := runMaybeCompact(t, r, sid, model)

	// One compaction entry appended.
	if countCompactions(t, st, sid) != 1 {
		t.Fatalf("compactions after = %d, want 1", countCompactions(t, st, sid))
	}
	// Events: start then end, with the right info.
	if len(events) != 2 || events[0].Type != CompactionStart || events[1].Type != CompactionEnd {
		t.Fatalf("events = %+v, want [start end]", events)
	}
	info := events[1].Compaction
	if info == nil || info.Summary != "SUMMARY-OK" || info.FirstKeptSeq != 8 {
		t.Fatalf("compaction info = %+v, want summary=SUMMARY-OK firstKeptSeq=8", info)
	}
	// Derived context: summary is returned separately; msgs is the kept tail.
	summary, msgs, _ := r.buildContext(context.Background(), sid)
	if !strings.Contains(summary, "SUMMARY-OK") {
		t.Fatalf("summary = %q, want SUMMARY-OK", summary)
	}
	if len(msgs) != 1 {
		t.Fatalf("derived context = %d msgs, want 1 (tail only)", len(msgs))
	}
}

// Below the headroom, no compaction happens.
func TestL2NotTriggered(t *testing.T) {
	st, sid := seedStore(t)
	appendMsgEntry(t, st, sid, NewUserMessage("hi"))
	r := &Runner{
		Store: st,
		Compaction: CompactionConfig{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 100, MinMessages: 2,
			Summarize: func(context.Context, Model, string) (string, error) { return "X", nil }},
	}
	model := Model{Provider: "p", ID: "m", ContextWindow: 100000} // huge budget
	events := runMaybeCompact(t, r, sid, model)
	if len(events) != 0 {
		t.Fatalf("no compaction expected, got %d events", len(events))
	}
	if countCompactions(t, st, sid) != 0 {
		t.Fatalf("no compaction entry expected")
	}
}

// Fail-safe (DESIGN §6.4): a summarizer error abandons the compaction — no
// entry is written and no compaction_end is emitted.
func TestL2CompactionFailSafe(t *testing.T) {
	st, sid := seedStore(t)
	for i := 0; i < 8; i++ {
		appendMsgEntry(t, st, sid, bigUser(600))
	}
	r := &Runner{
		Store: st,
		Compaction: CompactionConfig{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 100, MinMessages: 2,
			Summarize: func(context.Context, Model, string) (string, error) {
				return "", errors.New("model blew up")
			}},
	}
	model := Model{Provider: "p", ID: "m", ContextWindow: 500}
	events := runMaybeCompact(t, r, sid, model)
	if countCompactions(t, st, sid) != 0 {
		t.Fatalf("failed summarization must not write a compaction entry")
	}
	for _, ev := range events {
		if ev.Type == CompactionEnd {
			t.Fatalf("no compaction_end expected after a failure, got %+v", ev)
		}
	}
}

// Iteration: a second compaction re-feeds the prior summary via
// <previous-summary> and only summarizes the newly-grown live tail (not the
// already-folded older messages).
func TestL2IterativeSummary(t *testing.T) {
	st, sid := seedStore(t)
	for i := 0; i < 8; i++ {
		appendMsgEntry(t, st, sid, bigUser(600)) // seq 1..8
	}
	var firstPrompt string
	r := &Runner{
		Store: st,
		Compaction: CompactionConfig{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 100, MinMessages: 2,
			Summarize: func(_ context.Context, _ Model, prompt string) (string, error) {
				if firstPrompt == "" {
					firstPrompt = prompt
				}
				return "SUMMARY-V1", nil
			}},
	}
	model := Model{Provider: "p", ID: "m", ContextWindow: 500}
	runMaybeCompact(t, r, sid, model) // first: folds seq 1..7, keeps seq 8

	// Grow the context with more messages so it's over budget again.
	for i := 0; i < 8; i++ {
		appendMsgEntry(t, st, sid, bigUser(600)) // seq 9..16
	}
	var secondPrompt string
	r.Compaction.Summarize = func(_ context.Context, _ Model, prompt string) (string, error) {
		secondPrompt = prompt
		return "SUMMARY-V2", nil
	}
	runMaybeCompact(t, r, sid, model)

	if countCompactions(t, st, sid) != 2 {
		t.Fatalf("compactions = %d, want 2", countCompactions(t, st, sid))
	}
	// First prompt has no <previous-summary>; second does, carrying V1.
	if strings.Contains(firstPrompt, "<previous-summary>") {
		t.Fatalf("first prompt should not carry a previous summary: %q", firstPrompt)
	}
	if !strings.Contains(secondPrompt, "<previous-summary>") || !strings.Contains(secondPrompt, "SUMMARY-V1") {
		t.Fatalf("second prompt must carry the previous summary (V1):\n%s", secondPrompt)
	}
	// The derived context: summary (V2) is returned separately; msgs is the kept tail.
	summary, _, _ := r.buildContext(context.Background(), sid)
	if !strings.Contains(summary, "SUMMARY-V2") {
		t.Fatalf("summary = %q, want V2 summary", summary)
	}
}

// The cut point never leaves a tool result as the first kept message (its
// tool call would be summarized away). The kept tail must start at a user or
// assistant message.
func TestL2SafeCutBoundary(t *testing.T) {
	st, sid := seedStore(t)
	// 6 turns of (assistant-with-toolcall, tool-result), each assistant big.
	for i := 0; i < 6; i++ {
		id := "tc" + string(rune('0'+i))
		appendMsgEntry(t, st, sid, bigUser(600)) // a user msg to keep turns distinct
		asst := &Message{Role: RoleAssistant, Content: []Block{
			ToolCallBlock(id, "now", json.RawMessage(`{}`)),
		}}
		appendMsgEntry(t, st, sid, asst)
		appendMsgEntry(t, st, sid, NewToolResultMessage(ToolCall{ID: id, Name: "now"}, ToolResult{Output: strings.Repeat("r", 600)}))
	}
	var firstKept int64
	r := &Runner{
		Store: st,
		Compaction: CompactionConfig{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 100, MinMessages: 2,
			Summarize: func(context.Context, Model, string) (string, error) { return "S", nil }},
	}
	model := Model{Provider: "p", ID: "m", ContextWindow: 500}
	events := runMaybeCompact(t, r, sid, model)
	for _, ev := range events {
		if ev.Type == CompactionEnd && ev.Compaction != nil {
			firstKept = ev.Compaction.FirstKeptSeq
		}
	}
	if firstKept == 0 {
		t.Fatal("no compaction_end observed")
	}
	// The entry at firstKept must be a user or assistant, never a tool result.
	entries, _ := st.ListEntries(context.Background(), sid, 0, 0)
	for _, e := range entries {
		if e.Seq == firstKept {
			var m Message
			json.Unmarshal(e.Payload, &m)
			if m.Role == RoleTool {
				t.Fatalf("first kept entry (seq %d) is a tool result — orphaned boundary", firstKept)
			}
		}
	}
	// And the kept tail has no tool result lacking its tool call: the first
	// kept is user/assistant, so all kept tool results have their call kept.
}

// SummarizeWithStream adapts a StreamFn: text deltas concatenate; a tool
// call or stream error is surfaced as an error (the L2 fail-safe).
func TestSummarizeWithStream(t *testing.T) {
	textFn := func(context.Context, Request) (<-chan StreamEvent, error) {
		ch := make(chan StreamEvent)
		go func() {
			defer close(ch)
			ch <- StreamEvent{Type: StreamStart}
			ch <- StreamEvent{Type: StreamTextDelta, Text: "hello "}
			ch <- StreamEvent{Type: StreamTextDelta, Text: "world"}
			ch <- StreamEvent{Type: StreamDone}
		}()
		return ch, nil
	}
	s := SummarizeWithStream(textFn)
	out, err := s(context.Background(), Model{}, "prompt")
	if err != nil || out != "hello world" {
		t.Fatalf("text summarizer = %q, %v; want 'hello world', nil", out, err)
	}

	toolFn := func(context.Context, Request) (<-chan StreamEvent, error) {
		ch := make(chan StreamEvent)
		go func() {
			defer close(ch)
			ch <- StreamEvent{Type: StreamStart}
			ch <- StreamEvent{Type: StreamToolCallDelta, ToolCallID: "x", ToolName: "y", Text: "{}"}
			ch <- StreamEvent{Type: StreamDone}
		}()
		return ch, nil
	}
	if _, err := SummarizeWithStream(toolFn)(context.Background(), Model{}, "p"); err == nil {
		t.Fatalf("tool-call summarizer must return an error (fail-safe)")
	}

	errFn := func(context.Context, Request) (<-chan StreamEvent, error) {
		ch := make(chan StreamEvent)
		go func() {
			defer close(ch)
			ch <- StreamEvent{Type: StreamError, Err: errors.New("boom")}
		}()
		return ch, nil
	}
	if _, err := SummarizeWithStream(errFn)(context.Background(), Model{}, "p"); err == nil {
		t.Fatalf("error summarizer must return an error")
	}
}
