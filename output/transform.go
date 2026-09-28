package output

import (
	"context"
	"errors"

	"github.com/lee00jx/pi-go/core"
)

// FailurePolicy controls what Transform emits when a Processor or Stage
// rejects output. None of the policies alter the model message persisted by
// core.
type FailurePolicy uint8

const (
	// EmitError emits output_error and suppresses the rejected content. It
	// is the safe default: a validation/sanitizing stage may reject content
	// specifically because passing it through would be unsafe.
	EmitError FailurePolicy = iota
	// FallbackToRaw emits output_error and falls back to an ordinary
	// text_delta when the rejected input has a textual representation.
	FallbackToRaw
	// DropInvalid silently suppresses the rejected content.
	DropInvalid
)

type config struct {
	factory Factory
	stages  []Stage
	failure FailurePolicy
}

// Option configures Transform.
type Option func(*config)

// WithProcessor installs the project-defined output protocol parser. A nil
// factory disables processing and makes Transform return the source channel.
func WithProcessor(factory Factory) Option {
	return func(cfg *config) { cfg.factory = factory }
}

// WithStages appends validation/sanitizing stages in execution order. Nil
// stages are ignored.
func WithStages(stages ...Stage) Option {
	return func(cfg *config) {
		for _, stage := range stages {
			if stage != nil {
				cfg.stages = append(cfg.stages, stage)
			}
		}
	}
}

// WithFailurePolicy selects how processor/stage failures are surfaced.
func WithFailurePolicy(policy FailurePolicy) Option {
	return func(cfg *config) { cfg.failure = policy }
}

// Transform optionally post-processes model text while preserving all other
// core events and their order. When no Processor is configured it returns
// source directly and starts no goroutine.
func Transform(ctx context.Context, source <-chan core.AgentEvent, opts ...Option) <-chan core.AgentEvent {
	cfg := config{failure: EmitError}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.factory == nil {
		return source
	}

	out := make(chan core.AgentEvent, 64)
	go transform(ctx, source, out, cfg)
	return out
}

func transform(ctx context.Context, source <-chan core.AgentEvent, out chan<- core.AgentEvent, cfg config) {
	defer close(out)

	send := func(ev core.AgentEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var processor Processor
	bypass := false
	start := func() {
		processor = cfg.factory()
		bypass = processor == nil
	}
	reset := func() {
		processor = nil
		bypass = false
	}

	emitFailure := func(err error, fallback string) bool {
		switch cfg.failure {
		case DropInvalid:
			return true
		case FallbackToRaw:
			if !send(core.EvOutputError(err)) {
				return false
			}
			if fallback != "" {
				return send(core.EvTextDelta(fallback))
			}
			return true
		default:
			return send(core.EvOutputError(err))
		}
	}

	emitChunks := func(chunks []Chunk) bool {
		for _, chunk := range chunks {
			current := chunk
			var err error
			for _, stage := range cfg.stages {
				current, err = stage.Process(ctx, current)
				if err != nil {
					break
				}
			}
			if err == nil && current.Kind == "" {
				err = errors.New("output: processor produced a chunk without a kind")
			}
			if err != nil {
				fallback := current.Text
				if fallback == "" {
					fallback = string(current.Data)
				}
				if !emitFailure(err, fallback) {
					return false
				}
				continue
			}
			data := append([]byte(nil), current.Data...)
			if !send(core.EvOutput(current.Kind, current.Text, data)) {
				return false
			}
		}
		return true
	}

	finish := func() bool {
		if processor == nil || bypass {
			return true
		}
		chunks, err := processor.Finish(ctx)
		if err != nil {
			return emitFailure(err, "")
		}
		return emitChunks(chunks)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-source:
			if !ok {
				finish()
				return
			}

			switch ev.Type {
			case core.MessageStart:
				if ev.Message != nil && ev.Message.Role == core.RoleAssistant {
					start()
				}
				if !send(ev) {
					return
				}

			case core.TextDelta:
				if processor == nil {
					start()
				}
				if bypass {
					if !send(ev) {
						return
					}
					continue
				}
				chunks, err := processor.Write(ctx, ev.DeltaText)
				if err != nil {
					if !emitFailure(err, ev.DeltaText) {
						return
					}
					// A stateful parser may no longer be usable after an
					// error; pass through the rest of this message.
					bypass = true
					continue
				}
				if !emitChunks(chunks) {
					return
				}

			case core.MessageEnd:
				if ev.Message != nil && ev.Message.Role == core.RoleAssistant {
					if !finish() {
						return
					}
					reset()
				}
				if !send(ev) {
					return
				}

			default:
				if !send(ev) {
					return
				}
			}
		}
	}
}
