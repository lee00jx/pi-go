package core

import (
	"context"
	"encoding/json"
	"strings"
)

// Execution mode of a tool. A batch of tool calls runs in parallel unless
// it contains a sequential tool (parallel dispatch lands in phase 1;
// phase 0 executes in order, which already guarantees ordered backfill).
type Mode string

const (
	ModeParallel   Mode = "parallel"
	ModeSequential Mode = "sequential"
)

// ToolCall is one requested tool invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolUpdate is a streaming partial result from a running tool, emitted as
// tool_execution_update events.
type ToolUpdate struct {
	Output string `json:"output"`
}

// ToolResult is the outcome of one tool execution.
type ToolResult struct {
	Output string `json:"output"`
	// IsError marks the result as an error fed back to the model so it can
	// self-correct; the loop keeps running (DESIGN §2.3).
	IsError bool `json:"isError,omitempty"`
	// Terminate: when every tool result of a batch has this set, the loop
	// stops (mirrors pi's terminate semantics).
	Terminate bool            `json:"terminate,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// Tool is the contract every tool — built-in (bash/fs/recall_event) or
// business-registered — implements. sql is not provided (pi has none).
type Tool interface {
	Name() string
	// Schema returns the JSON Schema for arguments.
	Schema() json.RawMessage
	ExecutionMode() Mode
	// Execute runs the call. update may be called any number of times with
	// partial output while the tool runs.
	Execute(ctx context.Context, call ToolCall, update func(ToolUpdate)) (ToolResult, error)
}

// SessionTool is an optional extension of Tool for tools that must know which
// session a run belongs to. One Runner serves every session, but a tool like
// recall_event (the L1-archive escape hatch, compaction.go) has to read one
// specific session's store, so it needs the session id at execute time. The
// loop dispatches to ExecuteIn when a registered tool implements this
// interface; tools that do not are untouched and keep using Tool.Execute.
// This is non-breaking: adding the method changes no existing tool's contract.
type SessionTool interface {
	Tool
	ExecuteIn(ctx context.Context, sessionID string, call ToolCall, update func(ToolUpdate)) (ToolResult, error)
}

// DescribedTool is an optional extension of Tool. When implemented, providers
// send the string as the LLM-facing tool description (pi's ToolDefinition.description
// → OpenAI function.description / Anthropic tools[].description). Tools that
// omit it still work; the model then sees only name + parameter schema.
// This is non-breaking: adding Description() to Tool itself would force every
// existing implementer to change.
type DescribedTool interface {
	Tool
	Description() string
}

// ToolDescription returns t.Description() when t implements DescribedTool,
// otherwise "". Providers use this when building the wire tool list.
func ToolDescription(t Tool) string {
	if t == nil {
		return ""
	}
	d, ok := t.(DescribedTool)
	if !ok {
		return ""
	}
	return strings.TrimSpace(d.Description())
}

// Decision is the outcome of a BeforeToolCall hook.
type Decision struct {
	Block bool
	// Reason doubles as the human-facing risk description when Confirm is
	// set (it is carried in the tool_confirmation_request event).
	Reason string
	// Confirm routes the call to the confirmation flow (DESIGN §5.3):
	// the loop emits tool_confirmation_request, waits on the ConfirmTool
	// hook, and only executes when the user allows.
	Confirm bool
	// Terminate stops the loop once this (blocking) decision is fed back.
	Terminate bool
}

// Confirmation is the user's answer to a confirmation request.
type Confirmation struct {
	Allowed bool
	// Reason explains a denial (or the timeout); empty on allow.
	Reason string
}
