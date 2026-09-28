package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/session"
)

// ErrSessionRunning is returned by InjectTurn when the session is mid-run.
// Injecting ui_event rows alongside a live run would interleave with the
// real event stream; callers should wait until idle / interrupted.
var ErrSessionRunning = errors.New("gateway: session is running")

// ErrInvalidInject is returned when InjectTurnInput fails validation.
var ErrInvalidInject = errors.New("gateway: invalid inject turn")

// InjectTurnInput describes one synthetic turn that does not call the LLM.
// The gateway writes a user message, an assistant message (for buildContext),
// and an output ui_event so GET /turns has a non-empty results area.
// Hospital welcome / ticket copy stays in the host — this API is generic.
type InjectTurnInput struct {
	UserText      string          // required: user bubble text
	AssistantText string          // EntryAssistant body; required unless OutputText is set
	OutputText    string          // output event text for /turns results; empty → AssistantText
	OutputKind    string          // optional; default "inject"
	OutputData    json.RawMessage // optional opaque pointer metadata
	PromptID      string          // optional; empty → newPromptID()
}

// InjectTurn appends one complete turn under a single prompt_id without
// starting a run. Auth matches other session APIs (401 / 404). Only
// idle or interrupted sessions are accepted (running → ErrSessionRunning).
// No synthetic run_id or agent_end is written: under idle, /turns marks
// the turn complete without those frames.
func (g *Gateway) InjectTurn(ctx context.Context, sessionID string, in InjectTurnInput) (promptID string, err error) {
	meta, err := g.sessionFor(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if meta.Status == session.StatusRunning {
		return "", ErrSessionRunning
	}

	userText := strings.TrimSpace(in.UserText)
	assistantText := strings.TrimSpace(in.AssistantText)
	outputText := strings.TrimSpace(in.OutputText)
	if userText == "" {
		return "", fmtInvalidInject("userText is required")
	}
	if assistantText == "" && outputText == "" {
		return "", fmtInvalidInject("assistantText or outputText is required")
	}
	if assistantText == "" {
		assistantText = outputText
	}
	if outputText == "" {
		outputText = assistantText
	}
	kind := strings.TrimSpace(in.OutputKind)
	if kind == "" {
		kind = "inject"
	}
	promptID = strings.TrimSpace(in.PromptID)
	if promptID == "" {
		promptID = newPromptID()
	}

	user := core.NewUserMessage(userText)
	user.PromptID = promptID
	userPayload, err := json.Marshal(user)
	if err != nil {
		return "", err
	}
	if _, err := g.store.AppendEntry(ctx, sessionID, session.EntryUser, userPayload, nil); err != nil {
		return "", err
	}

	asst := &core.Message{
		Role:       core.RoleAssistant,
		Content:    []core.Block{core.TextBlock(assistantText)},
		StopReason: core.StopEndTurn,
		PromptID:   promptID,
	}
	asstPayload, err := json.Marshal(asst)
	if err != nil {
		return "", err
	}
	if _, err := g.store.AppendEntry(ctx, sessionID, session.EntryAssistant, asstPayload, nil); err != nil {
		return "", err
	}

	ev := core.EvOutput(kind, outputText, in.OutputData)
	ev.PromptID = promptID
	evPayload, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	if _, err := g.store.AppendEntry(ctx, sessionID, session.EntryUIEvent, evPayload, nil); err != nil {
		return "", err
	}
	return promptID, nil
}

func fmtInvalidInject(msg string) error {
	return errors.Join(ErrInvalidInject, errors.New(msg))
}
