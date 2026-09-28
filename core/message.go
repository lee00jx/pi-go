// Package core implements the agent's ReAct loop, event model, tool
// contract and hooks.
//
// Layering discipline (DESIGN §1): core depends only on the standard
// library and the session package (storage contract). No web or ORM
// frameworks, ever. The unified streaming types (StreamFn/StreamEvent)
// live here too, so provider clients depend on core, not the other way
// around — this is the one deliberate inversion of DESIGN §1's arrow.
package core

import (
	"encoding/json"
	"strings"
)

// Role classifies a message by sender.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Content block kinds.
const (
	BlockText     = "text"
	BlockThinking = "thinking"
	BlockToolCall = "toolCall"
)

// Block is one content block of a message. Only the fields matching Type
// are meaningful:
//
//	text      → Text
//	thinking  → Thinking (plus Signature, Anthropic-only, see below)
//	toolCall  → ToolCallID, Name, Arguments
//
// Signature carries the provider's opaque thinking signature (Anthropic)
// so an extended-thinking conversation can round-trip to the same
// provider. Other providers ignore it; converting a thinking block to a
// different provider drops both Thinking and Signature (DESIGN §3.5).
type Block struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
	Signature  string          `json:"signature,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
}

func TextBlock(s string) Block { return Block{Type: BlockText, Text: s} }

func ThinkingBlock(s string) Block { return Block{Type: BlockThinking, Thinking: s} }

func ToolCallBlock(id, name string, args json.RawMessage) Block {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return Block{Type: BlockToolCall, ToolCallID: id, Name: name, Arguments: args}
}

// Usage is provider-reported token accounting for one assistant message.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// Assistant stop reasons.
const (
	StopEndTurn = "end_turn"
	StopLength  = "length"
	StopToolUse = "tool_use"
	StopError   = "error"
	StopAborted = "aborted"
)

// Message is the neutral, provider-agnostic message model. It is persisted
// verbatim (append-only) and converted to each provider's wire format at
// the LLM boundary (convertToLlm, phase 2).
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`

	// Tool-role fields.
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`

	// Assistant fields.
	StopReason string `json:"stopReason,omitempty"`
	Model      string `json:"model,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`

	// PromptID identifies one user submission (including a queued steer).
	// It is assigned by the gateway and persisted with the user message;
	// request_id remains HTTP-only idempotency and does not replace it.
	PromptID string `json:"promptId,omitempty"`
}

// NewUserMessage builds a plain-text user message.
func NewUserMessage(text string) *Message {
	return &Message{Role: RoleUser, Content: []Block{TextBlock(text)}}
}

// NewToolResultMessage wraps one tool result as a tool-role message.
func NewToolResultMessage(call ToolCall, res ToolResult) *Message {
	return &Message{
		Role:       RoleTool,
		Content:    []Block{TextBlock(res.Output)},
		ToolCallID: call.ID,
		ToolName:   call.Name,
		IsError:    res.IsError,
	}
}

// Text returns the concatenated text blocks of the message (empty for
// tool-call-only assistant messages).
func (m *Message) Text() string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// ToolCalls extracts the tool call blocks in message order.
func (m *Message) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, b := range m.Content {
		if b.Type == BlockToolCall {
			out = append(out, ToolCall{ID: b.ToolCallID, Name: b.Name, Arguments: b.Arguments})
		}
	}
	return out
}
