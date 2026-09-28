package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/session"
)

// seedStore makes a memory store with one session ready for entries.
func seedStore(t *testing.T) (session.Store, string) {
	t.Helper()
	st := session.NewMemoryStore()
	sid := "s1"
	if err := st.CreateSession(context.Background(), session.SessionMeta{ID: sid, UserID: "u", Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return st, sid
}

func appendMsgEntry(t *testing.T, st session.Store, sid string, m *Message) int64 {
	t.Helper()
	var typ session.EntryType
	switch m.Role {
	case RoleUser:
		typ = session.EntryUser
	case RoleAssistant:
		typ = session.EntryAssistant
	default:
		typ = session.EntryToolResult
	}
	payload, _ := json.Marshal(m)
	seq, err := st.AppendEntry(context.Background(), sid, typ, payload, nil)
	if err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
	return seq
}

// appendCompaction seeds a type=compaction entry and returns its seq.
func appendCompaction(t *testing.T, st session.Store, sid string, c CompactionEntry) int64 {
	t.Helper()
	payload, _ := json.Marshal(c)
	seq, err := st.AppendEntry(context.Background(), sid, session.EntryCompaction, payload, nil)
	if err != nil {
		t.Fatalf("append compaction: %v", err)
	}
	return seq
}

// The latest compaction folds earlier messages into its summary; buildContext
// returns the summary separately (for the system prompt) + the kept tail
// (seq >= FirstKeptSeq). The summary is NOT a standalone user message (#1 fix).
func TestBuildContextCompactionSplice(t *testing.T) {
	st, sid := seedStore(t)
	// seq 1..5
	appendMsgEntry(t, st, sid, NewUserMessage("q1"))                                                                        // 1
	appendMsgEntry(t, st, sid, &Message{Role: RoleAssistant, Content: []Block{TextBlock("a1")}})                            // 2
	appendMsgEntry(t, st, sid, NewToolResultMessage(ToolCall{ID: "t1", Name: "now"}, ToolResult{Output: "r1"}))             // 3
	appendMsgEntry(t, st, sid, NewUserMessage("q2"))                                                                        // 4
	appendMsgEntry(t, st, sid, &Message{Role: RoleAssistant, Content: []Block{TextBlock("a2")}})                            // 5
	appendCompaction(t, st, sid, CompactionEntry{Summary: "OLD SUMMARY", FirstKeptSeq: 4, TokensBefore: 999, Model: "p/m"}) // 6

	r := &Runner{Store: st, Compaction: DefaultCompaction()}
	summary, msgs, _ := r.buildContext(context.Background(), sid)

	// Summary is returned separately, not embedded in msgs.
	if summary != "OLD SUMMARY" {
		t.Fatalf("summary = %q, want OLD SUMMARY", summary)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2 (q2 + a2): %+v", len(msgs), msgs)
	}
	if msgs[0].Text() != "q2" || msgs[1].Text() != "a2" {
		t.Fatalf("kept tail = %q / %q, want q2 / a2", msgs[0].Text(), msgs[1].Text())
	}
}

// No compaction entry + Enabled → plain replay (unchanged behavior).
func TestBuildContextNoCompaction(t *testing.T) {
	st, sid := seedStore(t)
	appendMsgEntry(t, st, sid, NewUserMessage("q1"))
	appendMsgEntry(t, st, sid, &Message{Role: RoleAssistant, Content: []Block{TextBlock("a1")}})

	r := &Runner{Store: st, Compaction: DefaultCompaction()}
	_, msgs, _ := r.buildContext(context.Background(), sid)
	if len(msgs) != 2 || msgs[0].Text() != "q1" || msgs[1].Text() != "a1" {
		t.Fatalf("msgs = %+v, want [q1 a1]", msgs)
	}
}

// Compaction disabled (zero config) ignores any compaction entry and replays
// everything (phase 0 behavior preserved for opt-out).
func TestBuildContextCompactionDisabled(t *testing.T) {
	st, sid := seedStore(t)
	appendMsgEntry(t, st, sid, NewUserMessage("q1"))
	appendCompaction(t, st, sid, CompactionEntry{Summary: "X", FirstKeptSeq: 2})

	r := &Runner{Store: st} // zero Compaction = disabled
	_, msgs, _ := r.buildContext(context.Background(), sid)
	if len(msgs) != 1 || msgs[0].Text() != "q1" {
		t.Fatalf("disabled: msgs = %+v, want [q1] (compaction ignored)", msgs)
	}
}

// applyL1 replaces a tool result from more than N user turns ago with the
// archive placeholder; recent tool results are kept.
func TestApplyL1(t *testing.T) {
	msgs := []*Message{
		NewUserMessage("q1"), // 0
		&Message{Role: RoleAssistant, Content: []Block{ToolCallBlock("t1", "now", nil)}},        // 1
		NewToolResultMessage(ToolCall{ID: "t1", Name: "now"}, ToolResult{Output: "OLD-RESULT"}), // 2
		NewUserMessage("q2"), // 3
		&Message{Role: RoleAssistant, Content: []Block{ToolCallBlock("t2", "now", nil)}},        // 4
		NewToolResultMessage(ToolCall{ID: "t2", Name: "now"}, ToolResult{Output: "NEW-RESULT"}), // 5
	}
	seqs := []int64{1, 2, 3, 4, 5, 6}

	out, _ := applyL1(msgs, seqs, 1)
	// tool@idx2 has 1 user (q2) after it → 1 >= 1 → evicted.
	if out[2].Text() != archivePlaceholder(3) {
		t.Fatalf("evicted tool = %q, want %q", out[2].Text(), archivePlaceholder(3))
	}
	// tool@idx5 has 0 users after it → kept.
	if out[5].Text() != "NEW-RESULT" {
		t.Fatalf("recent tool = %q, want NEW-RESULT", out[5].Text())
	}
	// Tool identity is preserved in the placeholder message.
	if out[2].ToolCallID != "t1" || out[2].Role != RoleTool {
		t.Fatalf("placeholder identity = %+v", out[2])
	}
}

// evictOlderThan <= 0 is a no-op.
func TestApplyL1Disabled(t *testing.T) {
	msgs := []*Message{
		NewUserMessage("q1"),
		NewToolResultMessage(ToolCall{ID: "t", Name: "n"}, ToolResult{Output: "keep"}),
		NewUserMessage("q2"),
	}
	seqs := []int64{1, 2, 3}
	if out, _ := applyL1(msgs, seqs, 0); out[1].Text() != "keep" {
		t.Fatalf("disabled L1 should not evict: %q", out[1].Text())
	}
}

// L1 through buildContext: a stale tool result is archived in the derived
// context while the store still holds the original.
func TestBuildContextL1(t *testing.T) {
	st, sid := seedStore(t)
	appendMsgEntry(t, st, sid, NewUserMessage("q1"))                                                                       // 1
	appendMsgEntry(t, st, sid, &Message{Role: RoleAssistant, Content: []Block{ToolCallBlock("t", "now", nil)}})            // 2
	appendMsgEntry(t, st, sid, NewToolResultMessage(ToolCall{ID: "t", Name: "now"}, ToolResult{Output: "BIG-OLD-RESULT"})) // 3
	appendMsgEntry(t, st, sid, NewUserMessage("q2"))                                                                       // 4
	appendMsgEntry(t, st, sid, NewUserMessage("q3"))                                                                       // 5

	r := &Runner{Store: st, Compaction: DefaultCompaction()} // EvictOlderThan=5
	// Lower the threshold so the tool result (2 users after it) is evicted.
	r.Compaction.EvictOlderThan = 2
	_, msgs, _ := r.buildContext(context.Background(), sid)

	// Find the tool message; it should now be the placeholder.
	var found bool
	for _, m := range msgs {
		if m.Role == RoleTool {
			found = true
			if !strings.Contains(m.Text(), "已归档") {
				t.Fatalf("tool not evicted: %q", m.Text())
			}
		}
	}
	if !found {
		t.Fatal("no tool message in derived context")
	}
	// The store still has the original (append-only: L1 never rewrites).
	all, _ := st.ListEntries(context.Background(), sid, 0, 0)
	for _, e := range all {
		if e.Type == session.EntryToolResult {
			var m Message
			json.Unmarshal(e.Payload, &m)
			if m.Text() != "BIG-OLD-RESULT" {
				t.Fatalf("store rewritten! = %q", m.Text())
			}
		}
	}
}
