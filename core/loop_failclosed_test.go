package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/lee00jx/pi-go/session"
)

var errStoreDown = errors.New("store down")

// brokenListStore embeds MemoryStore but ListEntries always fails with a
// store error (not a ctx cancellation): the fail-closed path under test.
type brokenListStore struct {
	*session.MemoryStore
}

func (s *brokenListStore) ListEntries(_ context.Context, _ string, _ int64, _ int) ([]session.Entry, error) {
	return nil, errStoreDown
}

// A store read failure at a turn boundary must ABORT the turn (#6 fix),
// not silently hand the model an empty context. Regression: the old
// buildContext swallowed ListEntries errors as ("", nil), and the loop
// called the LLM with zero history.
func TestBuildContextFailureAbortsTurn(t *testing.T) {
	st, sid := seedStore(t)
	appendMsgEntry(t, st, sid, NewUserMessage("q1"))

	broken := &brokenListStore{MemoryStore: st.(*session.MemoryStore)}

	var calls atomic.Int32
	fn := StreamFn(func(ctx context.Context, req Request) (<-chan StreamEvent, error) {
		calls.Add(1)
		ch := make(chan StreamEvent, 2)
		go func() {
			defer close(ch)
			ch <- StreamEvent{Type: StreamStart, Message: &Message{Role: RoleAssistant}}
			ch <- StreamEvent{Type: StreamDone, Message: &Message{Role: RoleAssistant, Content: []Block{TextBlock("answered-from-nothing")}}}
		}()
		return ch, nil
	})

	r := &Runner{Store: broken, StreamFn: fn}
	events, rerr := r.Run(context.Background(), sid, []*Message{NewUserMessage("hi")})
	if rerr != nil {
		t.Fatalf("Run: %v", rerr)
	}

	var ended *AgentEvent
	for ev := range events {
		if ev.Type == AgentEnd {
			e := ev
			ended = &e
		}
	}
	if ended == nil {
		t.Fatal("run did not emit agent_end")
	}
	if calls.Load() != 0 {
		t.Fatalf("StreamFn called %d times on a failed context read; want 0 (fail-closed)", calls.Load())
	}
	if len(ended.Messages) != 1 || ended.Messages[0].Role != RoleUser {
		t.Fatalf("agent_end messages = %+v, want only the user prompt", ended.Messages)
	}
}

// RunContinue on a failing store surfaces the error instead of
// continuing from a silent empty context.
func TestRunContinueFailingStore(t *testing.T) {
	st, sid := seedStore(t)
	appendMsgEntry(t, st, sid, NewUserMessage("q1"))
	broken := &brokenListStore{MemoryStore: st.(*session.MemoryStore)}

	r := &Runner{Store: broken, StreamFn: func(ctx context.Context, req Request) (<-chan StreamEvent, error) {
		return nil, errors.New("unreachable")
	}}
	if _, err := r.RunContinue(context.Background(), sid); !errors.Is(err, errStoreDown) {
		t.Fatalf("RunContinue err = %v, want errStoreDown", err)
	}
}
