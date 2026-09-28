package core

import "encoding/json"

// AgentEvent kinds (DESIGN §2.2).
//
// The JSON wire format of AgentEvent is a public contract (semver-managed):
// the Web frontend renders exclusively from this stream, and the gateway
// persists every event as a ui_event entry so SSE replay reproduces it
// exactly.
// Event type constants. The Ev* functions below are the matching
// constructors; the constants carry no prefix.
const (
	AgentStart          = "agent_start"
	AgentEnd            = "agent_end"
	TurnStart           = "turn_start"
	TurnEnd             = "turn_end"
	MessageStart        = "message_start"
	MessageUpdate       = "message_update" // reserved: partial snapshots (phase 1)
	MessageEnd          = "message_end"
	TextDelta           = "text_delta"
	ThinkingDelta       = "thinking_delta"
	ToolCallDelta       = "toolcall_delta"
	ToolExecStart       = "tool_execution_start"
	ToolExecUpdate      = "tool_execution_update"
	ToolExecEnd         = "tool_execution_end"
	ToolConfirmRequest  = "tool_confirmation_request"
	ToolConfirmResponse = "tool_confirmation_response"
	CompactionStart     = "compaction_start" // phase 5
	CompactionEnd       = "compaction_end"   // phase 5
	ModelChanged        = "model_changed"
	ContextFull         = "context_full"     // phase 5
	BudgetExhausted     = "budget_exhausted" // phase 5
	Output              = "output"           // optional output processor chunk
	OutputError         = "output_error"     // optional output processor failure
)

// AgentEvent is one agent behavior. Only the fields relevant to Type are
// set; consumers switch on Type.
type AgentEvent struct {
	Type string `json:"type"`

	// Correlation IDs. Session identity lives on the store row; seq is
	// assigned at persist time. PromptID is one user submission; RunID is
	// one Agent execution. The gateway stamps both after EventMiddleware.
	PromptID string `json:"promptId,omitempty"`
	RunID    string `json:"runId,omitempty"`

	// turn_*
	Turn int `json:"turn,omitempty"`

	// message_start / message_end
	Message *Message `json:"message,omitempty"`

	// turn_end
	ToolResults []*Message `json:"toolResults,omitempty"`

	// agent_end: every message produced by this run.
	Messages []*Message `json:"messages,omitempty"`

	// text_delta / thinking_delta / toolcall_delta
	DeltaKind string `json:"deltaKind,omitempty"`
	DeltaText string `json:"deltaText,omitempty"`

	// tool_execution_* / toolcall_delta
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     *ToolResult     `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`

	// tool_confirmation_*
	ConfirmationID string `json:"confirmationId,omitempty"`
	Risk           string `json:"risk,omitempty"`
	// tool_confirmation_response: the user's decision.
	Reason string `json:"reason,omitempty"`

	// compaction_end (phase 5)
	Compaction *CompactionInfo `json:"compaction,omitempty"`

	// model_changed
	Model *Model `json:"model,omitempty"`

	// output / output_error. OutputKind is integrator-defined; core treats
	// OutputData as an opaque payload.
	OutputKind string          `json:"outputKind,omitempty"`
	OutputText string          `json:"outputText,omitempty"`
	OutputData json.RawMessage `json:"outputData,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// CompactionInfo describes a completed L2 compaction (DESIGN §6.4).
type CompactionInfo struct {
	Summary      string `json:"summary"`
	FirstKeptSeq int64  `json:"firstKeptSeq"`
	TokensBefore int    `json:"tokensBefore"`
}

// Event constructors keep call sites readable and the wire shape stable.

func EvAgentStart() AgentEvent { return AgentEvent{Type: AgentStart} }

func EvAgentEnd(msgs []*Message) AgentEvent {
	return AgentEvent{Type: AgentEnd, Messages: msgs}
}

func EvTurnStart(turn int) AgentEvent { return AgentEvent{Type: TurnStart, Turn: turn} }

func EvTurnEnd(turn int, msg *Message, toolResults []*Message) AgentEvent {
	return AgentEvent{Type: TurnEnd, Turn: turn, Message: msg, ToolResults: toolResults}
}

func EvMessageStart(m *Message) AgentEvent { return AgentEvent{Type: MessageStart, Message: m} }

func EvMessageEnd(m *Message) AgentEvent { return AgentEvent{Type: MessageEnd, Message: m} }

func EvTextDelta(text string) AgentEvent {
	return AgentEvent{Type: TextDelta, DeltaKind: BlockText, DeltaText: text}
}

func EvThinkingDelta(text string) AgentEvent {
	return AgentEvent{Type: ThinkingDelta, DeltaKind: BlockThinking, DeltaText: text}
}

func EvToolCallDelta(text, toolCallID, toolName string) AgentEvent {
	return AgentEvent{
		Type: ToolCallDelta, DeltaKind: BlockToolCall, DeltaText: text,
		ToolCallID: toolCallID, ToolName: toolName,
	}
}

func EvToolExecStart(call ToolCall) AgentEvent {
	return AgentEvent{Type: ToolExecStart, ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments}
}

func EvToolExecUpdate(call ToolCall, partial ToolUpdate) AgentEvent {
	return AgentEvent{
		Type: ToolExecUpdate, ToolCallID: call.ID, ToolName: call.Name,
		Args: call.Arguments, Result: &ToolResult{Output: partial.Output},
	}
}

func EvToolExecEnd(call ToolCall, res ToolResult) AgentEvent {
	return AgentEvent{
		Type: ToolExecEnd, ToolCallID: call.ID, ToolName: call.Name,
		Result: &res, IsError: res.IsError,
	}
}

// EvOutput carries one integrator-defined chunk produced by an optional
// output processor. Core does not interpret kind, text, or data.
func EvOutput(kind, text string, data json.RawMessage) AgentEvent {
	return AgentEvent{
		Type: Output, OutputKind: kind, OutputText: text, OutputData: data,
	}
}

// EvOutputError reports an optional output pipeline failure without changing
// the model message or terminating the agent loop.
func EvOutputError(err error) AgentEvent {
	if err == nil {
		return AgentEvent{Type: OutputError}
	}
	return AgentEvent{Type: OutputError, Error: err.Error()}
}

func EvModelChanged(m Model) AgentEvent { return AgentEvent{Type: ModelChanged, Model: &m} }

// Phase 5: compaction + resource governance events.

func EvCompactionStart() AgentEvent { return AgentEvent{Type: CompactionStart} }

func EvCompactionEnd(info *CompactionInfo) AgentEvent {
	return AgentEvent{Type: CompactionEnd, Compaction: info}
}

// EvContextFull signals the context window is at capacity (no compaction
// possible) — the front-end can surface a "context full" warning.
func EvContextFull() AgentEvent { return AgentEvent{Type: ContextFull} }

// EvBudgetExhausted signals the session's token budget was hit; the loop
// stops after the current turn.
func EvBudgetExhausted() AgentEvent { return AgentEvent{Type: BudgetExhausted} }

// EvToolConfirmRequest announces that a tool call awaits user confirmation
// (DESIGN §5.3). ConfirmationID is the tool call's id — it is what the
// frontend posts back to the confirmations endpoint.
func EvToolConfirmRequest(call ToolCall, risk string) AgentEvent {
	return AgentEvent{
		Type: ToolConfirmRequest, ToolCallID: call.ID, ToolName: call.Name,
		Args: call.Arguments, ConfirmationID: call.ID, Risk: risk,
	}
}

// EvToolConfirmResponse reports the decision (IsError = denied/timed out).
// Persisting it is what makes the decision survive refreshes (DESIGN §5.3).
func EvToolConfirmResponse(call ToolCall, conf Confirmation) AgentEvent {
	return AgentEvent{
		Type: ToolConfirmResponse, ToolCallID: call.ID, ToolName: call.Name,
		ConfirmationID: call.ID, IsError: !conf.Allowed, Reason: conf.Reason,
	}
}
