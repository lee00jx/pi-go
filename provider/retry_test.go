package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
)

// scripted is a StreamFn that plays one scripted stream per call, counting
// invocations.
type scripted struct {
	calls   *int
	streams [][]core.StreamEvent
}

func (s *scripted) Stream(ctx context.Context, _ core.Request) (<-chan core.StreamEvent, error) {
	i := *s.calls
	*s.calls++
	if i >= len(s.streams) {
		ch := make(chan core.StreamEvent, 1)
		ch <- core.StreamEvent{Type: core.StreamError, Err: errors.New("script exhausted")}
		close(ch)
		return ch, nil
	}
	evs := s.streams[i]
	ch := make(chan core.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func okStream() []core.StreamEvent {
	return []core.StreamEvent{
		{Type: core.StreamStart, Message: &core.Message{Role: core.RoleAssistant}},
		{Type: core.StreamTextDelta, Text: "hi"},
		{Type: core.StreamDone, Message: &core.Message{Role: core.RoleAssistant, Content: []core.Block{core.TextBlock("hi")}, StopReason: core.StopEndTurn}},
	}
}

func errStream(status int, retryable bool) []core.StreamEvent {
	return []core.StreamEvent{{Type: core.StreamError, Err: &APIError{Status: status, Message: "rate limited", Retryable: retryable}}}
}

func fast(t *testing.T) *RetryConfig {
	t.Helper()
	return &RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestRetryThenSuccess(t *testing.T) {
	var calls int
	fn := &scripted{calls: &calls, streams: [][]core.StreamEvent{errStream(429, true), errStream(429, true), okStream()}}
	wrapped := WithRetry(fn.Stream, fast(t))

	ch, err := wrapped(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var evs []core.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	if calls != 3 {
		t.Fatalf("attempts = %d, want 3", calls)
	}
	// Exactly one start (retries must not re-emit), one text, one done.
	starts, dones := 0, 0
	var text string
	for _, ev := range evs {
		switch ev.Type {
		case core.StreamStart:
			starts++
		case core.StreamTextDelta:
			text += ev.Text
		case core.StreamDone:
			dones++
		case core.StreamError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if starts != 1 || dones != 1 || text != "hi" {
		t.Fatalf("events: starts=%d dones=%d text=%q", starts, dones, text)
	}
}

func TestRetryFastFailsOn400(t *testing.T) {
	var calls int
	fn := &scripted{calls: &calls, streams: [][]core.StreamEvent{errStream(400, false), okStream()}}
	wrapped := WithRetry(fn.Stream, fast(t))
	ch, _ := wrapped(context.Background(), core.Request{})
	var gotErr bool
	for ev := range ch {
		if ev.Type == core.StreamError {
			gotErr = true
			var ae *APIError
			if !errors.As(ev.Err, &ae) || ae.Status != 400 {
				t.Fatalf("error = %v, want the 400 APIError", ev.Err)
			}
		}
	}
	if !gotErr {
		t.Fatal("expected an error event")
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx must not retry)", calls)
	}
}

func TestNoRetryAfterContent(t *testing.T) {
	var calls int
	partial := []core.StreamEvent{
		{Type: core.StreamStart, Message: &core.Message{Role: core.RoleAssistant}},
		{Type: core.StreamTextDelta, Text: "par"},
		{Type: core.StreamError, Err: &APIError{Status: 500, Message: "boom", Retryable: true},
			Message: &core.Message{Role: core.RoleAssistant, Content: []core.Block{core.TextBlock("par")}}},
	}
	fn := &scripted{calls: &calls, streams: [][]core.StreamEvent{partial, okStream()}}
	wrapped := WithRetry(fn.Stream, fast(t))
	ch, _ := wrapped(context.Background(), core.Request{})

	var sawPartial, sawError bool
	var text string
	for ev := range ch {
		switch ev.Type {
		case core.StreamTextDelta:
			text += ev.Text
		case core.StreamError:
			sawError = true
			sawPartial = ev.Message != nil && ev.Message.Text() == "par"
		}
	}
	if !sawError || !sawPartial {
		t.Fatalf("want error with partial message, sawErr=%v sawPartial=%v", sawError, sawPartial)
	}
	if text != "par" {
		t.Fatalf("text = %q, want only the partial (no retry)", text)
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry after content)", calls)
	}
}

func TestFallbackAfterExhaustion(t *testing.T) {
	var primaryCalls, fbCalls int
	primary := &scripted{calls: &primaryCalls, streams: [][]core.StreamEvent{errStream(429, true), errStream(429, true)}}
	fbModel := core.Model{Provider: "deepseek", ID: "deepseek-chat", ContextWindow: 64000}
	fallback := &scripted{calls: &fbCalls, streams: [][]core.StreamEvent{okStream()}}

	var notified core.Model
	cfg := fast(t)
	cfg.MaxRetries = 1
	cfg.Fallback = fallback.Stream
	cfg.FallbackModel = &fbModel
	cfg.OnFallback = func(m core.Model) { notified = m }

	wrapped := WithRetry(primary.Stream, cfg)
	ch, _ := wrapped(context.Background(), core.Request{Model: core.Model{Provider: "openai", ID: "gpt-4o"}})

	var done *core.StreamEvent
	for ev := range ch {
		if ev.Type == core.StreamDone {
			done = &ev
		}
	}
	if primaryCalls != 2 {
		t.Fatalf("primary attempts = %d, want 2", primaryCalls)
	}
	if fbCalls != 1 {
		t.Fatalf("fallback attempts = %d, want 1", fbCalls)
	}
	if notified.ID != "deepseek-chat" {
		t.Fatalf("OnFallback got %v, want deepseek-chat", notified)
	}
	if done == nil || done.Model == nil || done.Model.ID != "deepseek-chat" {
		t.Fatalf("done.Model = %+v, want the fallback model tagged", done)
	}
}

func TestNoFallbackOn400(t *testing.T) {
	var fbCalls int
	primary := &scripted{calls: new(int), streams: [][]core.StreamEvent{errStream(400, false)}}
	cfg := fast(t)
	cfg.Fallback = (&scripted{calls: &fbCalls, streams: [][]core.StreamEvent{okStream()}}).Stream
	wrapped := WithRetry(primary.Stream, cfg)
	ch, _ := wrapped(context.Background(), core.Request{})
	for range ch {
	}
	if fbCalls != 0 {
		t.Fatalf("fallback attempted %d times, want 0 (4xx is not a retryable exhaustion)", fbCalls)
	}
}

func TestContextCancelStopsRetrying(t *testing.T) {
	var calls int
	fn := &scripted{calls: &calls, streams: [][]core.StreamEvent{errStream(429, true), errStream(429, true), errStream(429, true), errStream(429, true)}}
	wrapped := WithRetry(fn.Stream, fast(t))
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := wrapped(ctx, core.Request{})
	cancel()
	// Drain whatever arrives; the wrapper must terminate promptly.
	deadline := make(chan struct{})
	go func() {
		for range ch {
		}
		close(deadline)
	}()
	select {
	case <-deadline:
	case <-time.After(2 * time.Second):
		t.Fatal("wrapper did not terminate after ctx cancel")
	}
}
