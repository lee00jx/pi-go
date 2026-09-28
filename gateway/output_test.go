package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/output"
	"github.com/lee00jx/pi-go/provider"
)

type uppercaseProcessor struct{}

func (uppercaseProcessor) Write(_ context.Context, delta string) ([]output.Chunk, error) {
	return []output.Chunk{{Kind: "upper", Text: strings.ToUpper(delta)}}, nil
}

func (uppercaseProcessor) Finish(context.Context) ([]output.Chunk, error) {
	return nil, nil
}

func TestEventMiddlewarePersistsProcessedOutput(t *testing.T) {
	middleware := func(ctx context.Context, source <-chan core.AgentEvent) <-chan core.AgentEvent {
		return output.Transform(
			ctx,
			source,
			output.WithProcessor(func() output.Processor { return uppercaseProcessor{} }),
		)
	}
	_, _, srv, _ := newRecoverFixture(
		t,
		[]provider.FauxTurn{{Text: "hello"}},
		WithEventMiddleware(middleware),
	)
	base := srv.URL + "/api/agent/sessions/s1"
	postJSON(t, base+"/prompts", `{"text":"go"}`)

	frames, err := readSSEUntil(t, base+"/events?after=0", core.AgentEnd, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var sawOutput, sawRawText bool
	for _, frame := range frames {
		var event core.AgentEvent
		if err := json.Unmarshal([]byte(frame), &event); err != nil {
			continue
		}
		switch event.Type {
		case core.Output:
			if event.OutputKind == "upper" && event.OutputText == "HELLO" {
				sawOutput = true
			}
		case core.TextDelta:
			sawRawText = true
		}
	}
	if !sawOutput {
		t.Fatal("processed output event was not persisted and replayed")
	}
	if sawRawText {
		t.Fatal("raw text_delta leaked through the configured output processor")
	}
}
