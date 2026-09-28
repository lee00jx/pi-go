package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lee00jx/pi-go/core"
)

// Anthropic is a hand-written Messages API client (DESIGN §3.2): there is
// no official Go SDK, and the SSE event format, tool-call structure,
// thinking blocks and usage split are Anthropic-specific.
type Anthropic struct {
	// BaseURL defaults to https://api.anthropic.com (no trailing slash).
	BaseURL string
	APIKey  string

	// Provider label used in error messages (defaults to "anthropic").
	Provider string

	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client
}

const (
	anthropicAPIVersion = "2023-06-01"
	anDefaultMaxTokens  = 8192 // the API requires max_tokens; this is the fallback
)

// NewAnthropic validates the minimum configuration.
func NewAnthropic(apiKey string) (*Anthropic, error) {
	if apiKey == "" {
		return nil, errors.New("provider: anthropic apiKey is required")
	}
	return &Anthropic{BaseURL: "https://api.anthropic.com", APIKey: apiKey, Provider: "anthropic"}, nil
}

// wire types (messages API).

type anRequest struct {
	Model       string      `json:"model"`
	MaxTokens   int         `json:"max_tokens"`
	System      []anText    `json:"system,omitempty"`
	Messages    []anMessage `json:"messages"`
	Tools       []anTool    `json:"tools,omitempty"`
	Thinking    *anThinking `json:"thinking,omitempty"`
	Stream      bool        `json:"stream"`
	Temperature *float64    `json:"temperature,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
}

type anText struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// anMessage.Content is a JSON string (plain user text) or a block array.
type anMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// block shapes for message history (see buildAnMessages).

type anToolUseBlock struct {
	Type  string          `json:"type"` // "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anThinkingBlock struct {
	Type      string `json:"type"` // "thinking"
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type anToolResultBlock struct {
	Type      string `json:"type"` // "tool_result"
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// SSE event shapes (one struct, fields optional per event type).

type anUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

type anEventBlock struct {
	Type      string          `json:"type"` // text | thinking | redacted_thinking | tool_use
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"` // redacted_thinking payload
	ID        string          `json:"id,omitempty"`   // tool_use
	Name      string          `json:"name,omitempty"` // tool_use
	Input     json.RawMessage `json:"input,omitempty"`
}

type anStopDetails struct {
	Category    string `json:"category,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

type anEventDelta struct {
	// content_block_delta:
	Type        string `json:"type"` // text_delta | thinking_delta | input_json_delta | signature_delta
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Signature   string `json:"signature,omitempty"`
	// message_delta:
	StopReason  *string        `json:"stop_reason,omitempty"`
	StopDetails *anStopDetails `json:"stop_details,omitempty"`
}

type anEvent struct {
	Type         string        `json:"type"`
	Message      *anEventMsg   `json:"message,omitempty"`
	Index        int           `json:"index,omitempty"`
	ContentBlock *anEventBlock `json:"content_block,omitempty"`
	Delta        *anEventDelta `json:"delta,omitempty"`
	Usage        *anUsage      `json:"usage,omitempty"`
	Error        *anErrorBody  `json:"error,omitempty"`
}

type anEventMsg struct {
	Model string   `json:"model"`
	Usage *anUsage `json:"usage,omitempty"`
}

type anErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Stream implements core.StreamFn against the Messages API.
func (a *Anthropic) Stream(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
	if req.Model.ID == "" {
		return nil, errors.New("provider: anthropic model id is required")
	}
	label := a.Provider
	if label == "" {
		label = "anthropic"
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}

	maxTokens := req.Sampling.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anDefaultMaxTokens
	}
	anReq := anRequest{
		Model:       req.Model.ID,
		MaxTokens:   maxTokens,
		Messages:    buildAnMessages(req),
		Tools:       buildAnTools(req.Tools),
		Stream:      true,
		Temperature: req.Sampling.Temperature,
		TopP:        req.Sampling.TopP,
	}
	if req.SystemPrompt != "" {
		anReq.System = []anText{{Type: "text", Text: req.SystemPrompt}}
	}
	if req.ThinkingLevel != "" {
		anReq.Thinking = &anThinking{Type: "enabled", BudgetTokens: thinkingBudget(req.ThinkingLevel)}
	}
	body, err := json.Marshal(anReq)
	if err != nil {
		return nil, fmt.Errorf("provider: anthropic encode request: %w", err)
	}

	httpClient := a.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		// Transport error: retryable (WithRetry).
		return nil, &APIError{Provider: label, Message: err.Error(), Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		// The error body is {"type":"error","error":{"type","message"}}.
		var eb struct {
			Error anErrorBody `json:"error"`
		}
		message := strings.TrimSpace(string(msg))
		if json.Unmarshal(msg, &eb) == nil && eb.Error.Message != "" {
			message = eb.Error.Message
		}
		return nil, &APIError{
			Provider: label,
			Status:   resp.StatusCode,
			Message:  message,
			Retryable: resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusRequestTimeout ||
				resp.StatusCode >= 500,
		}
	}

	out := make(chan core.StreamEvent, 32)
	go a.readStream(ctx, resp, req.Model, out)
	return out, nil
}

// anAccum gathers one content block while it streams (keyed by the API's
// content_block index).
type anAccum struct {
	kind      string // text | thinking | toolCall
	text      string
	thinking  string
	signature string
	id, name  string
	args      strings.Builder
}

// readStream consumes the SSE response (event: + data: pairs) and emits
// normalized events. The event handling mirrors pi
// (packages/ai/src/api/anthropic-messages.ts) event for event.
func (a *Anthropic) readStream(ctx context.Context, resp *http.Response, model core.Model, out chan<- core.StreamEvent) {
	defer resp.Body.Close()
	label := a.Provider
	if label == "" {
		label = "anthropic"
	}
	send := func(ev core.StreamEvent) {
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}

	var blocks []anAccum
	posByIndex := map[int]int{}
	input, output, cacheRead, cacheWrite := 0, 0, 0, 0
	var stopReason string
	var stopErr error
	sawStart := false

	partialMessage := func() *core.Message {
		return assembleAnMessage(blocks, model, core.StopError)
	}
	applyUsage := func(u *anUsage) {
		// Pointers: the API sends null for fields it does not report
		// (e.g. input_tokens in message_delta), which must not clobber
		// the values from message_start.
		if u == nil {
			return
		}
		if u.InputTokens != nil {
			input = *u.InputTokens
		}
		if u.OutputTokens != nil {
			output = *u.OutputTokens
		}
		if u.CacheReadInputTokens != nil {
			cacheRead = *u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens != nil {
			cacheWrite = *u.CacheCreationInputTokens
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	curEvent := ""
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		switch {
		case line == "":
			curEvent = ""
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			var ev anEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				send(core.StreamEvent{Type: core.StreamError,
					Err:     &APIError{Provider: label, Message: fmt.Sprintf("bad SSE event: %v: %.256s", err, data), Retryable: false},
					Message: partialMessage()})
				return
			}
			name := ev.Type
			if name == "" {
				name = curEvent
			}
			switch name {
			case "message_start":
				if ev.Message != nil {
					applyUsage(ev.Message.Usage)
				}
				if !sawStart {
					sawStart = true
					send(core.StreamEvent{Type: core.StreamStart, Message: &core.Message{Role: core.RoleAssistant, Model: model.ID}})
				}
			case "content_block_start":
				cb := ev.ContentBlock
				if cb == nil {
					continue
				}
				acc := anAccum{}
				switch cb.Type {
				case "text":
					acc.kind = core.BlockText
					acc.text = cb.Text
					if cb.Text != "" {
						send(core.StreamEvent{Type: core.StreamTextDelta, Text: cb.Text})
					}
				case "thinking":
					acc.kind = core.BlockThinking
					acc.thinking = cb.Thinking
					acc.signature = cb.Signature
					if cb.Thinking != "" {
						send(core.StreamEvent{Type: core.StreamThinkingDelta, Text: cb.Thinking})
					}
				case "redacted_thinking":
					// Opaque payload; keep it as a redacted thinking block
					// so the signature round-trips on the next request.
					acc.kind = core.BlockThinking
					acc.thinking = "[Reasoning redacted]"
					acc.signature = cb.Data
				case "tool_use":
					acc.kind = core.BlockToolCall
					acc.id = cb.ID
					acc.name = cb.Name
				default:
					continue // unknown block type: skip, don't fail the stream
				}
				blocks = append(blocks, acc)
				posByIndex[ev.Index] = len(blocks) - 1
				// The API sends input:{} at start (always empty); a rare
				// non-empty start would be written through the slice —
				// the acc copy above still has a zero Builder, and a
				// Builder may not be copied once non-empty.
				if s := string(bytes.TrimSpace(cb.Input)); len(s) > 0 && s != "{}" {
					blocks[len(blocks)-1].args.Write(cb.Input)
				}
			case "content_block_delta":
				d := ev.Delta
				if d == nil {
					continue
				}
				pos, ok := posByIndex[ev.Index]
				if !ok {
					continue
				}
				acc := &blocks[pos]
				switch d.Type {
				case "text_delta":
					acc.text += d.Text
					send(core.StreamEvent{Type: core.StreamTextDelta, Text: d.Text})
				case "thinking_delta":
					acc.thinking += d.Thinking
					send(core.StreamEvent{Type: core.StreamThinkingDelta, Text: d.Thinking})
				case "input_json_delta":
					acc.args.WriteString(d.PartialJSON)
					// Stream the raw JSON fragment: the frontend renders it
					// live and the final message carries the salvage-parsed
					// object (mirrors the OpenAI client).
					send(core.StreamEvent{
						Type: core.StreamToolCallDelta,
						Text: d.PartialJSON, ToolCallID: acc.id, ToolName: acc.name,
					})
				case "signature_delta":
					acc.signature += d.Signature
					// No normalized event: the signature is metadata for the
					// next request, not display content.
				}
			case "message_delta":
				if ev.Delta != nil && ev.Delta.StopReason != nil {
					reason, err := mapStopReason(*ev.Delta.StopReason, ev.Delta.StopDetails)
					stopReason = reason
					stopErr = err
				}
				applyUsage(ev.Usage)
			case "message_stop":
				// Stream complete; the loop ends on body EOF.
			case "ping":
				// Keep-alive.
			case "error":
				var eb anErrorBody
				if ev.Error != nil {
					eb = *ev.Error
				}
				send(core.StreamEvent{
					Type: core.StreamError,
					Err: &APIError{
						Provider:  label,
						Message:   fmt.Sprintf("%s: %s", orDefault(eb.Type, "stream_error"), eb.Message),
						Retryable: anStreamErrorRetryable(eb.Type),
					},
					Message: partialMessage(),
				})
				return
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// Mid-stream disconnect: keep the partial, surface as a retryable
		// error (DESIGN §3.4).
		send(core.StreamEvent{
			Type:    core.StreamError,
			Err:     &APIError{Provider: label, Message: fmt.Sprintf("stream interrupted: %v", err), Retryable: true},
			Message: partialMessage(),
		})
		return
	}
	if ctx.Err() != nil {
		return
	}

	switch {
	case stopReason == "":
		// message_delta never arrived (pi: same error class).
		send(core.StreamEvent{
			Type:    core.StreamError,
			Err:     errors.New("anthropic: stream ended without a stop reason"),
			Message: partialMessage(),
		})
	case stopReason == core.StopError:
		// refusal / sensitive / unhandled: terminal, keep any partial.
		if stopErr == nil {
			stopErr = errors.New("anthropic: provider stopped the stream")
		}
		send(core.StreamEvent{Type: core.StreamError, Err: stopErr, Message: partialMessage()})
	default:
		msg := assembleAnMessage(blocks, model, stopReason)
		msg.Usage = &core.Usage{
			InputTokens:  input,
			OutputTokens: output,
			// Anthropic reports no total; it is the sum of the components
			// (cache tokens included), per pi.
			TotalTokens: input + output + cacheRead + cacheWrite,
		}
		send(core.StreamEvent{Type: core.StreamDone, Message: msg, Usage: msg.Usage})
	}
	close(out)
}

// mapStopReason mirrors pi's mapStopReason 1:1 (anthropic-messages.ts).
func mapStopReason(reason string, details *anStopDetails) (string, error) {
	switch reason {
	case "end_turn", "pause_turn", "stop_sequence":
		return core.StopEndTurn, nil
	case "max_tokens":
		return core.StopLength, nil
	case "tool_use":
		return core.StopToolUse, nil
	case "refusal":
		msg := "The model refused to complete the request"
		if details != nil && details.Explanation != "" {
			msg = details.Explanation
		}
		return core.StopError, errors.New(msg)
	case "sensitive":
		return core.StopError, errors.New("Provider stopped with: sensitive")
	default:
		// The API may add new reasons; fail loudly rather than guessing.
		return core.StopError, fmt.Errorf("unhandled stop reason: %s", reason)
	}
}

// anStreamErrorRetryable classifies in-stream error events.
func anStreamErrorRetryable(t string) bool {
	switch t {
	case "overloaded_error", "rate_limit_error", "api_error", "timeout_error":
		return true
	default:
		return false
	}
}

// buildAnMessages is the convertToLlm half of cross-provider history
// compatibility (DESIGN §3.5): neutral history → Anthropic wire.
//
// The Anthropic-specific shapes:
//   - tool results are USER messages carrying tool_result blocks; all
//     consecutive tool results merge into ONE user message (the API
//     requires alternating roles);
//   - thinking blocks round-trip WITH their signature (extended thinking
//     needs it); a signature-less thinking block (e.g. from another
//     provider or an aborted stream) downgrades to plain text — pi does
//     the same by default;
//   - tool calls are tool_use blocks {id, name, input}.
func buildAnMessages(req core.Request) []anMessage {
	msgs := make([]anMessage, 0, len(req.Messages))
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		switch m.Role {
		case core.RoleUser:
			text := m.Text()
			if strings.TrimSpace(text) == "" {
				continue
			}
			msgs = append(msgs, anMessage{Role: "user", Content: jsonString(text)})
		case core.RoleAssistant:
			var raw []json.RawMessage
			for _, b := range m.Content {
				switch b.Type {
				case core.BlockText:
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					raw = append(raw, mustJSON(anText{Type: "text", Text: b.Text}))
				case core.BlockThinking:
					if b.Signature != "" {
						raw = append(raw, mustJSON(anThinkingBlock{
							Type: "thinking", Thinking: b.Thinking, Signature: b.Signature,
						}))
					} else if strings.TrimSpace(b.Thinking) != "" {
						// No signature: the API would reject a thinking
						// block it cannot verify, so downgrade to text.
						raw = append(raw, mustJSON(anText{Type: "text", Text: b.Thinking}))
					}
				case core.BlockToolCall:
					args := b.Arguments
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					raw = append(raw, mustJSON(anToolUseBlock{
						Type: "tool_use", ID: b.ToolCallID, Name: b.Name, Input: args,
					}))
				}
			}
			if len(raw) == 0 {
				continue
			}
			joined, _ := json.Marshal(raw)
			msgs = append(msgs, anMessage{Role: "assistant", Content: joined})
		case core.RoleTool:
			// Merge this run of consecutive tool results into one user
			// message (pi: convertMessages toolResult branch).
			var results []anToolResultBlock
			for i < len(req.Messages) && req.Messages[i].Role == core.RoleTool {
				results = append(results, anToolResultBlock{
					Type:      "tool_result",
					ToolUseID: req.Messages[i].ToolCallID,
					Content:   req.Messages[i].Text(),
					IsError:   req.Messages[i].IsError,
				})
				i++
			}
			i-- // the outer loop advances
			blocksJSON, _ := json.Marshal(results)
			msgs = append(msgs, anMessage{Role: "user", Content: blocksJSON})
		}
	}
	return msgs
}

func buildAnTools(tools []core.Tool) []anTool {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]anTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema()
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		defs = append(defs, anTool{
			Name:        t.Name(),
			Description: core.ToolDescription(t),
			InputSchema: schema,
		})
	}
	return defs
}

// assembleAnMessage finalizes the streamed blocks into one assistant
// message, in block order. Truncated tool-call arguments are finalized
// with the salvage parser (DESIGN §2.3); truncation protection in the
// loop keeps them from executing.
func assembleAnMessage(blocks []anAccum, model core.Model, stopReason string) *core.Message {
	var out []core.Block
	for _, b := range blocks {
		switch b.kind {
		case core.BlockText:
			if b.text == "" {
				continue
			}
			out = append(out, core.TextBlock(b.text))
		case core.BlockThinking:
			out = append(out, core.Block{
				Type: core.BlockThinking, Thinking: b.thinking, Signature: b.signature,
			})
		case core.BlockToolCall:
			args := map[string]any{}
			if b.args.Len() > 0 {
				args, _ = core.ParseSalvage(b.args.String())
			}
			raw, _ := json.Marshal(args)
			out = append(out, core.ToolCallBlock(b.id, b.name, raw))
		}
	}
	return &core.Message{
		Role: core.RoleAssistant, Content: out, Model: model.ID,
		StopReason: stopReason,
	}
}

func thinkingBudget(level string) int {
	switch level {
	case "low", "minimal":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 16384
	default:
		return 4096
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
