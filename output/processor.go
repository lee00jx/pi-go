// Package output provides an optional, protocol-agnostic post-processing
// pipeline for core.AgentEvent streams. Projects supply a Processor for
// their own text protocol and optional Stages for validation or sanitizing;
// pi-go supplies the lifecycle and channel plumbing.
package output

import (
	"context"
	"encoding/json"
)

// Chunk is one project-defined output unit. Kind is an opaque name such as
// "text", "table", or "chart"; pi-go does not interpret it. A processor may
// set Text, Data, or both.
type Chunk struct {
	Kind string
	Text string
	Data json.RawMessage
}

// Processor incrementally translates model text into project-defined chunks.
// One Processor instance is created per assistant message, so implementations
// may safely keep partial-tag and other stream state on the instance.
type Processor interface {
	Write(ctx context.Context, delta string) ([]Chunk, error)
	Finish(ctx context.Context) ([]Chunk, error)
}

// Factory creates a fresh Processor for one assistant message.
type Factory func() Processor

// Stage is one optional post-processing step. Returning an error rejects the
// chunk according to the Transform failure policy. Validation stages return
// the chunk unchanged; sanitizers return a rewritten chunk.
type Stage interface {
	Process(ctx context.Context, chunk Chunk) (Chunk, error)
}

// StageFunc adapts a function to Stage.
type StageFunc func(context.Context, Chunk) (Chunk, error)

// Process implements Stage.
func (f StageFunc) Process(ctx context.Context, chunk Chunk) (Chunk, error) {
	return f(ctx, chunk)
}
