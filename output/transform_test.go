package output

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/core"
)

type testProcessor struct {
	finish []Chunk
	err    error
}

func (p *testProcessor) Write(_ context.Context, delta string) ([]Chunk, error) {
	if p.err != nil {
		return nil, p.err
	}
	return []Chunk{{Kind: "text", Text: strings.ToUpper(delta)}}, nil
}

func (p *testProcessor) Finish(context.Context) ([]Chunk, error) {
	return p.finish, nil
}

func eventSource(events ...core.AgentEvent) <-chan core.AgentEvent {
	ch := make(chan core.AgentEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func collect(events <-chan core.AgentEvent) []core.AgentEvent {
	var out []core.AgentEvent
	for event := range events {
		out = append(out, event)
	}
	return out
}

func TestTransformWithoutProcessorIsIdentity(t *testing.T) {
	source := make(chan core.AgentEvent)
	if got := Transform(context.Background(), source); got != source {
		t.Fatal("Transform without a processor must return the source channel")
	}
	close(source)
}

func TestTransformProcessesOnlyTextAndRunsStagesInOrder(t *testing.T) {
	assistant := &core.Message{Role: core.RoleAssistant}
	var factories int
	factory := func() Processor {
		factories++
		return &testProcessor{finish: []Chunk{{
			Kind: "meta",
			Data: json.RawMessage(`{"closed":true}`),
		}}}
	}
	appendText := func(suffix string) Stage {
		return StageFunc(func(_ context.Context, chunk Chunk) (Chunk, error) {
			chunk.Text += suffix
			return chunk, nil
		})
	}

	got := collect(Transform(
		context.Background(),
		eventSource(
			core.EvMessageStart(assistant),
			core.EvTextDelta("hello"),
			core.EvThinkingDelta("private"),
			core.EvMessageEnd(assistant),
		),
		WithProcessor(factory),
		WithStages(appendText("-one"), appendText("-two")),
	))

	if factories != 1 {
		t.Fatalf("processor factories = %d, want 1", factories)
	}
	if len(got) != 5 {
		t.Fatalf("events = %#v, want 5", got)
	}
	if got[0].Type != core.MessageStart || got[1].Type != core.Output ||
		got[1].OutputKind != "text" || got[1].OutputText != "HELLO-one-two" {
		t.Fatalf("processed text events = %#v", got)
	}
	if got[2].Type != core.ThinkingDelta || got[2].DeltaText != "private" {
		t.Fatalf("thinking event was not passed through: %#v", got[2])
	}
	if got[3].Type != core.Output || got[3].OutputKind != "meta" ||
		string(got[3].OutputData) != `{"closed":true}` {
		t.Fatalf("finish event = %#v", got[3])
	}
	if got[4].Type != core.MessageEnd {
		t.Fatalf("last event = %#v, want message_end", got[4])
	}
}

func TestTransformFallbackToRawAfterProcessorError(t *testing.T) {
	assistant := &core.Message{Role: core.RoleAssistant}
	got := collect(Transform(
		context.Background(),
		eventSource(
			core.EvMessageStart(assistant),
			core.EvTextDelta("first"),
			core.EvTextDelta("second"),
			core.EvMessageEnd(assistant),
		),
		WithProcessor(func() Processor {
			return &testProcessor{err: errors.New("bad protocol")}
		}),
		WithFailurePolicy(FallbackToRaw),
	))

	if len(got) != 5 {
		t.Fatalf("events = %#v, want 5", got)
	}
	if got[1].Type != core.OutputError || got[1].Error != "bad protocol" {
		t.Fatalf("error event = %#v", got[1])
	}
	if got[2].Type != core.TextDelta || got[2].DeltaText != "first" ||
		got[3].Type != core.TextDelta || got[3].DeltaText != "second" {
		t.Fatalf("raw fallback events = %#v", got[2:4])
	}
}

func TestTransformDefaultStageFailureEmitsError(t *testing.T) {
	assistant := &core.Message{Role: core.RoleAssistant}
	reject := StageFunc(func(_ context.Context, chunk Chunk) (Chunk, error) {
		return chunk, errors.New("rejected")
	})
	got := collect(Transform(
		context.Background(),
		eventSource(
			core.EvMessageStart(assistant),
			core.EvTextDelta("unsafe"),
			core.EvMessageEnd(assistant),
		),
		WithProcessor(func() Processor { return &testProcessor{} }),
		WithStages(reject),
	))

	if len(got) != 3 || got[1].Type != core.OutputError || got[1].Error != "rejected" {
		t.Fatalf("events = %#v, want message_start/output_error/message_end", got)
	}
}
