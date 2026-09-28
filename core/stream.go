package core

import "context"

// Model identifies the LLM to call.
type Model struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	ContextWindow int    `json:"contextWindow"`
}

func (m Model) String() string { return m.Provider + "/" + m.ID }

// Ptr is a tiny helper so Sampling fields can be set without a temporary
// variable: Sampling{Temperature: Ptr(0.7)}. A nil pointer means "omit —
// let the server use its default", which is how an integrator who is
// happy with defaults leaves the field.
func Ptr[T any](v T) *T { return &v }

// Sampling is the optional generation knobs for one LLM call (and, on
// Runner, the agent-wide default copied onto every turn). The zero value
// means "send nothing extra": vLLM / OpenAI / Anthropic keep their own
// defaults. Pointers are used for Temperature, TopP and Seed so a literal
// 0 is distinguishable from "unset" (greedy decoding and seed=0 are both
// valid). MaxTokens is a plain int because 0 is never a useful cap.
//
// Only the knobs every OpenAI-compatible server (including vLLM) honours
// live here. presence_penalty / frequency_penalty / stop / top_k stay out
// of the public contract — they are provider-specific and easy to add
// later without breaking this struct.
type Sampling struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	Seed        *int64
}

// Normalized stream event kinds (DESIGN §3.3):
//
//	start → (text_delta | thinking_delta | toolcall_delta)* → done | error
const (
	StreamStart         = "start"
	StreamTextDelta     = "text_delta"
	StreamThinkingDelta = "thinking_delta"
	StreamToolCallDelta = "toolcall_delta"
	StreamDone          = "done"
	StreamError         = "error"
)

// StreamEvent is one step of a normalized LLM stream. Providers (real or
// faux) all emit this shape; core only depends on it.
type StreamEvent struct {
	Type string

	// start / done: the (growing) assistant message.
	Message *Message

	// Deltas. For text_delta / thinking_delta: Text. For toolcall_delta:
	// the streamed JSON-fragment in Text plus the call's identity.
	Text       string
	ToolCallID string
	ToolName   string

	// done: provider usage, including the provider-native total tokens.
	Usage *Usage

	// done: set when the provider actually used a model different from
	// Request.Model (e.g. retry exhausted and a fallback kicked in,
	// DESIGN §3.4). Core emits a model_changed event for it.
	Model *Model

	// error.
	Err error
}

// Request is everything a provider needs for one assistant turn.
type Request struct {
	Model         Model
	SystemPrompt  string
	Messages      []*Message
	Tools         []Tool
	ThinkingLevel string
	// Sampling is copied from Runner.Sampling at the start of each turn.
	// Zero value = omit every knob (server defaults). Anthropic still
	// requires max_tokens on the wire; the client fills 8192 when
	// Sampling.MaxTokens is 0.
	Sampling Sampling
}

// StreamFn produces the normalized event stream for one assistant turn.
// The channel always closes after a done or error event.
//
// Implementations: provider.Faux (scripted, exported for testing),
// OpenAI-compatible and Anthropic clients (phases 1-2).
type StreamFn func(ctx context.Context, req Request) (<-chan StreamEvent, error)
