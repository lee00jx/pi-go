// Package provider contains LLM clients that satisfy core.StreamFn.
//
// Phase 0 ships Faux (scripted) only; the OpenAI-compatible client lands
// in phase 1, the hand-written Anthropic client in phase 2.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/lee00jx/pi-go/core"
)

// FauxToolCall is one scripted tool invocation.
type FauxToolCall struct {
	// ID is auto-generated (deterministically) when empty.
	ID   string
	Name string
	Args json.RawMessage // raw JSON object; {} when empty
}

// FauxTurn is one scripted assistant turn.
type FauxTurn struct {
	Thinking  string
	Text      string
	ToolCalls []FauxToolCall
	Usage     *core.Usage
	// StopReason overrides the default (tool_use when ToolCalls are set,
	// otherwise end_turn). Script core.StopLength to exercise truncation
	// protection.
	StopReason string
	// Err, when non-nil, ends the turn with a stream error.
	Err error
}

// Faux is a scripted provider: each Stream call replays the next turn,
// holding on the last turn once the script is exhausted. It is exported as
// a public contract so library tests AND integrator tests run without
// spending a single token (DESIGN §9).
type Faux struct {
	Model core.Model
	Turns []FauxTurn

	mu  sync.Mutex
	idx int
}

// NewFaux builds a scripted provider. model.Provider defaults to "faux".
func NewFaux(model core.Model, turns ...FauxTurn) *Faux {
	if model.Provider == "" {
		model.Provider = "faux"
	}
	return &Faux{Model: model, Turns: turns}
}

// Stream implements core.StreamFn.
func (f *Faux) Stream(ctx context.Context, _ core.Request) (<-chan core.StreamEvent, error) {
	if len(f.Turns) == 0 {
		return nil, errors.New("provider: faux has no scripted turns")
	}
	f.mu.Lock()
	turn := f.Turns[f.idx]
	turnIdx := f.idx
	if f.idx < len(f.Turns)-1 {
		f.idx++
	}
	f.mu.Unlock()

	// Resolve tool-call ids up front so the message blocks and the
	// toolcall_delta events always agree.
	calls := make([]core.ToolCall, 0, len(turn.ToolCalls))
	for i, tc := range turn.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("faux_%d_%d", turnIdx, i)
		}
		args := tc.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		calls = append(calls, core.ToolCall{ID: id, Name: tc.Name, Arguments: args})
	}

	out := make(chan core.StreamEvent, 32)
	go func() {
		defer close(out)
		send := func(ev core.StreamEvent) {
			select {
			case out <- ev:
			case <-ctx.Done():
			}
		}
		if turn.Err != nil {
			send(core.StreamEvent{Type: core.StreamError, Err: turn.Err})
			return
		}

		var blocks []core.Block
		if turn.Thinking != "" {
			blocks = append(blocks, core.ThinkingBlock(turn.Thinking))
		}
		if turn.Text != "" {
			blocks = append(blocks, core.TextBlock(turn.Text))
		}
		for _, c := range calls {
			blocks = append(blocks, core.ToolCallBlock(c.ID, c.Name, c.Arguments))
		}
		stop := turn.StopReason
		if stop == "" {
			stop = core.StopEndTurn
			if len(calls) > 0 {
				stop = core.StopToolUse
			}
		}
		usage := turn.Usage
		if usage == nil {
			usage = &core.Usage{InputTokens: 10, OutputTokens: 10, TotalTokens: 20}
		}
		final := &core.Message{
			Role: core.RoleAssistant, Content: blocks,
			StopReason: stop, Model: f.Model.ID, Usage: usage,
		}

		send(core.StreamEvent{Type: core.StreamStart, Message: final})
		if turn.Thinking != "" {
			send(core.StreamEvent{Type: core.StreamThinkingDelta, Text: turn.Thinking})
		}
		text := turn.Text
		for len(text) > 0 {
			n := 12
			if len(text) < n {
				n = len(text)
			}
			send(core.StreamEvent{Type: core.StreamTextDelta, Text: text[:n]})
			text = text[n:]
		}
		for _, c := range calls {
			send(core.StreamEvent{
				Type: core.StreamToolCallDelta, Text: string(c.Arguments),
				ToolCallID: c.ID, ToolName: c.Name,
			})
		}
		send(core.StreamEvent{Type: core.StreamDone, Message: final, Usage: usage})
	}()
	return out, nil
}
