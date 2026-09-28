package core

import (
	"context"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/session"
)

// TestBuildContextWrapsToolOutput verifies the prompt-injection baseline:
// the derived context (what the model sees) wraps tool output in a boundary,
// while the store keeps the raw output untouched (so recall_event returns the
// original). This is the "存什么 ≠ 喂什么" guarantee.
func TestBuildContextWrapsToolOutput(t *testing.T) {
	ctx := context.Background()
	store, sid := seedStore(t)

	appendMsgEntry(t, store, sid, NewUserMessage("hi"))
	raw := "ORIGINAL-TOOL-OUTPUT"
	appendMsgEntry(t, store, sid, NewToolResultMessage(ToolCall{ID: "c1", Name: "bash"}, ToolResult{Output: raw}))

	r := &Runner{Store: store}
	_, msgs, _ := r.buildContext(ctx, sid)

	var fed string
	for _, m := range msgs {
		if m.Role == RoleTool {
			fed = m.Text()
		}
	}
	if !strings.Contains(fed, raw) {
		t.Fatalf("fed context missing raw output: %q", fed)
	}
	if !strings.Contains(fed, "tool output") {
		t.Fatalf("fed context missing the injection boundary: %q", fed)
	}

	// The store must NOT contain the boundary — it is feed-only.
	entries, err := store.ListEntries(ctx, sid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Type == session.EntryToolResult && strings.Contains(string(e.Payload), "tool output —") {
			t.Fatalf("store leaked the injection boundary into stored payload: %s", e.Payload)
		}
	}
}
