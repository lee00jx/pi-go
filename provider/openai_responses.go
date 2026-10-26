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
	"sort"
	"strings"

	"github.com/lee00jx/pi-go/core"
)

// OpenAIResponses is a client for OpenAI's Responses API (/v1/responses).
//
// It is a separate client rather than a mode flag on OpenAI because the
// two APIs differ in almost every wire detail: input items instead of chat
// messages, flattened function tools, output-item stream events, and
// reasoning items that must be replayed verbatim on the next turn. The
// normalized side is the same core.StreamFn contract, so core does not
// care which client produced the stream.
type OpenAIResponses struct {
	// BaseURL ends without a trailing slash, e.g. https://api.openai.com/v1.
	BaseURL string
	APIKey  string

	// Provider label used in error messages (defaults to "openai").
	Provider string

	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client

	// ReasoningSummary asks OpenAI for a human-readable reasoning summary
	// ("auto", "concise", "detailed"; empty defaults to "auto"). It only
	// applies when a thinking level is set.
	ReasoningSummary string

	extraBody map[string]json.RawMessage
}

// OpenAIResponsesOption is the shared OpenAI option set. HTTP client, TLS
// and extra-body options work exactly as they do for the chat client.
type OpenAIResponsesOption = OpenAIOption

// NewOpenAIResponses validates the minimum configuration.
func NewOpenAIResponses(baseURL, apiKey string, opts ...OpenAIResponsesOption) (*OpenAIResponses, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("provider: openai-responses baseURL is required")
	}
	if apiKey == "" {
		return nil, errors.New("provider: openai-responses apiKey is required")
	}
	cfg, err := parseOpenAIOptions(opts)
	if err != nil {
		return nil, err
	}
	return &OpenAIResponses{
		BaseURL: baseURL, APIKey: apiKey, Provider: "openai",
		HTTPClient: cfg.httpClient, ReasoningSummary: "auto",
		extraBody: cfg.extraBody,
	}, nil
}

// --- wire types ---------------------------------------------------------

type respInputMessage struct {
	// Type is set to "message" for replayed assistant messages; user and
	// system messages omit it.
	Type    string `json:"type,omitempty"`
	Role    string `json:"role"`
	Content any    `json:"content"`
	Status  string `json:"status,omitempty"`
}

type respInputContent struct {
	Type string `json:"type"` // input_text | output_text
	Text string `json:"text"`
}

type respFunctionCall struct {
	Type      string `json:"type"` // function_call
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type respFunctionCallOutput struct {
	Type   string `json:"type"` // function_call_output
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type respTool struct {
	Type        string          `json:"type"` // function
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type respReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type respRequest struct {
	Model           string         `json:"model"`
	Input           []any          `json:"input"`
	Tools           []respTool     `json:"tools,omitempty"`
	Stream          bool           `json:"stream"`
	Store           bool           `json:"store"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Reasoning       *respReasoning `json:"reasoning,omitempty"`
	Include         []string       `json:"include,omitempty"`
}

// respEvent is the common shape of every Responses SSE event. Only the
// fields relevant to a given type are populated.
type respEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Delta       string          `json:"delta"`
	Arguments   string          `json:"arguments"`
	Item        json.RawMessage `json:"item"`
	Response    json.RawMessage `json:"response"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
}

type respItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []respSummary `json:"summary"`
	Content          []respContent `json:"content"`
}

type respSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type respContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type respUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type respIncomplete struct {
	Reason string `json:"reason"`
}

type respError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type respPayload struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Usage             *respUsage        `json:"usage"`
	IncompleteDetails *respIncomplete   `json:"incomplete_details"`
	Error             *respError        `json:"error"`
	Output            []json.RawMessage `json:"output"`
}

// --- request building ---------------------------------------------------

// Stream implements core.StreamFn against the Responses API.
func (o *OpenAIResponses) Stream(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
	if req.Model.ID == "" {
		return nil, errors.New("provider: openai-responses model id is required")
	}
	body, err := o.marshalRequest(o.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("provider: openai-responses encode request: %w", err)
	}

	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	label := o.Provider
	if label == "" {
		label = "openai"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, &APIError{Provider: label, Message: err.Error(), Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &APIError{
			Provider: label,
			Status:   resp.StatusCode,
			Message:  strings.TrimSpace(string(msg)),
			Retryable: resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusRequestTimeout ||
				resp.StatusCode >= 500,
		}
	}

	out := make(chan core.StreamEvent, 32)
	go o.readResponseStream(ctx, resp, req.Model, out)
	return out, nil
}

func (o *OpenAIResponses) buildRequest(req core.Request) respRequest {
	r := respRequest{
		Model:       req.Model.ID,
		Input:       buildRespInput(req),
		Tools:       buildRespTools(req.Tools),
		Stream:      true,
		Store:       false,
		Temperature: req.Sampling.Temperature,
		TopP:        req.Sampling.TopP,
	}
	if req.Sampling.MaxTokens > 0 {
		// Responses rejects max_output_tokens below 16 (OpenAI's own
		// floor); clamping here keeps a sampling value of 1 from failing.
		r.MaxOutputTokens = max(req.Sampling.MaxTokens, 16)
	}
	r.Reasoning, r.Include = respReasoningConfig(req.ThinkingLevel, o.ReasoningSummary)
	return r
}

// respReasoningConfig maps the neutral thinking level to the Responses
// reasoning object. Empty = omit (server default); "off" = effort "none";
// known levels additionally request a summary and the encrypted reasoning
// content needed to replay reasoning items on the next stateless turn.
func respReasoningConfig(level, summary string) (*respReasoning, []string) {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "":
		return nil, nil
	case "off", "none":
		return &respReasoning{Effort: "none"}, nil
	case "minimal", "low", "medium", "high", "xhigh", "max":
		summary = strings.TrimSpace(summary)
		if summary == "" {
			summary = "auto"
		}
		return &respReasoning{Effort: level, Summary: summary},
			[]string{"reasoning.encrypted_content"}
	default:
		// Unknown level: omit rather than send an invalid enum value.
		return nil, nil
	}
}

// buildRespInput translates neutral messages into Responses input items.
// Assistant messages are replayed as their original output items
// (reasoning → message → function_call); thinking-only assistant messages
// are skipped because an orphan reasoning item is rejected by OpenAI.
func buildRespInput(req core.Request) []any {
	items := make([]any, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		items = append(items, respInputMessage{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case core.RoleUser:
			items = append(items, respInputMessage{
				Role:    "user",
				Content: []respInputContent{{Type: "input_text", Text: m.Text()}},
			})
		case core.RoleAssistant:
			if !hasReplayableAssistantBlock(m) {
				continue
			}
			for _, b := range m.Content {
				switch b.Type {
				case core.BlockThinking:
					// Signature is the full Responses reasoning item,
					// including encrypted_content when reasoning was on.
					if isJSONObject(b.Signature) {
						items = append(items, json.RawMessage(b.Signature))
					}
				case core.BlockText:
					if b.Text == "" {
						continue
					}
					items = append(items, respInputMessage{
						Type: "message", Role: "assistant",
						Content: []respInputContent{{Type: "output_text", Text: b.Text}},
						Status:  "completed",
					})
				case core.BlockToolCall:
					args := b.Arguments
					if len(args) == 0 || !json.Valid(args) {
						args = json.RawMessage(`{}`)
					}
					items = append(items, respFunctionCall{
						Type: "function_call", ID: b.ItemID,
						CallID: b.ToolCallID, Name: b.Name, Arguments: string(args),
					})
				}
			}
		case core.RoleTool:
			items = append(items, respFunctionCallOutput{
				Type: "function_call_output", CallID: m.ToolCallID, Output: m.Text(),
			})
		}
	}
	return items
}

// hasReplayableAssistantBlock reports whether a message carries anything
// the Responses API can accept as output history. Thinking alone (even a
// signed reasoning item from an aborted turn) is not replayable: OpenAI
// rejects an orphan reasoning item, so such a message is dropped. A
// message with text or a tool call is kept, and its signed reasoning block
// travels with it.
func hasReplayableAssistantBlock(m *core.Message) bool {
	for _, b := range m.Content {
		switch b.Type {
		case core.BlockText:
			if b.Text != "" {
				return true
			}
		case core.BlockToolCall:
			return true
		}
	}
	return false
}

func isJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") && json.Valid([]byte(s))
}

func buildRespTools(tools []core.Tool) []respTool {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]respTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema()
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		defs = append(defs, respTool{
			Type: "function", Name: t.Name(),
			Description: core.ToolDescription(t), Parameters: schema,
		})
	}
	return defs
}

func (o *OpenAIResponses) marshalRequest(req respRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil || len(o.extraBody) == 0 {
		return body, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	for key, value := range o.extraBody {
		fields[key] = value
	}
	return json.Marshal(fields)
}

// --- stream parsing -----------------------------------------------------

type respCall struct {
	callID, itemID, name string
	args                 strings.Builder
}

type respThinking struct {
	id        string
	text      string
	signature json.RawMessage
}

// readResponseStream consumes the Responses SSE events and emits the
// normalized core.StreamEvent sequence. The terminal response event
// (response.completed / response.incomplete) is authoritative: its output
// array backfills anything the incremental events missed and its usage
// becomes the core.Usage.
func (o *OpenAIResponses) readResponseStream(ctx context.Context, resp *http.Response, model core.Model, out chan<- core.StreamEvent) {
	defer resp.Body.Close()
	defer close(out)
	label := o.Provider
	if label == "" {
		label = "openai"
	}
	send := func(ev core.StreamEvent) {
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}

	var text strings.Builder
	thinkings := make([]*respThinking, 0, 1)
	thinkingByIndex := map[int]*respThinking{}
	calls := map[int]*respCall{}
	var usage *core.Usage
	stopReason := core.StopEndTurn
	started := false
	terminal := false
	var streamErr error

	ensureStart := func() {
		if started {
			return
		}
		started = true
		send(core.StreamEvent{Type: core.StreamStart, Message: &core.Message{Role: core.RoleAssistant, Model: model.ID}})
	}
	thinkingFor := func(idx int) *respThinking {
		th := thinkingByIndex[idx]
		if th == nil {
			th = &respThinking{}
			thinkingByIndex[idx] = th
			thinkings = append(thinkings, th)
		}
		return th
	}
	callFor := func(idx int) *respCall {
		c := calls[idx]
		if c == nil {
			c = &respCall{}
			calls[idx] = c
		}
		return c
	}
	emitThinking := func(s string) {
		if s == "" {
			return
		}
		send(core.StreamEvent{Type: core.StreamThinkingDelta, Text: s})
	}
	emitText := func(s string) {
		if s == "" {
			return
		}
		send(core.StreamEvent{Type: core.StreamTextDelta, Text: s})
	}
	emitToolDelta := func(c *respCall, delta string) {
		if delta == "" {
			return
		}
		send(core.StreamEvent{
			Type: core.StreamToolCallDelta, Text: delta,
			ToolCallID: c.callID, ToolName: c.name,
		})
	}
	applyItem := func(idx int, raw json.RawMessage) {
		var item respItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return
		}
		switch item.Type {
		case "reasoning":
			th := thinkingFor(idx)
			if item.ID != "" {
				th.id = item.ID
			}
			if s := respReasoningText(item); s != "" {
				th.text = s
			}
			if len(th.signature) == 0 {
				// The added event often carries only the item id; wait for
				// done/completed to persist a reasoning item with real
				// payload (summary, content, or encrypted_content).
				if reasoningItemHasPayload(item) && len(raw) > 0 && isJSONObject(string(raw)) {
					th.signature = append(json.RawMessage(nil), raw...)
				}
			} else if item.EncryptedContent != "" {
				// Keep the item.done signature but fill in the encrypted
				// reasoning that some gateways only send at completion.
				th.signature = mergeEncryptedContent(th.signature, item.EncryptedContent)
			}
		case "message":
			if s := respContentText(item); s != "" {
				text.Reset()
				text.WriteString(s)
			}
		case "function_call":
			c := callFor(idx)
			if item.CallID != "" {
				c.callID = item.CallID
			}
			if item.ID != "" {
				c.itemID = item.ID
			}
			if item.Name != "" {
				c.name = item.Name
			}
			if item.Arguments != "" {
				setRespArgs(c, item.Arguments, emitToolDelta)
			}
		}
	}
	backfillReasoning := func(p *respPayload) {
		for _, raw := range p.Output {
			var item respItem
			if json.Unmarshal(raw, &item) != nil || item.Type != "reasoning" || item.ID == "" {
				continue
			}
			for _, th := range thinkings {
				if th.id != item.ID {
					continue
				}
				if len(th.signature) == 0 {
					if isJSONObject(string(raw)) {
						th.signature = append(json.RawMessage(nil), raw...)
					}
				} else if item.EncryptedContent != "" {
					th.signature = mergeEncryptedContent(th.signature, item.EncryptedContent)
				}
			}
		}
	}
	finishStream := func() {
		if !terminal && streamErr == nil {
			streamErr = &APIError{
				Provider:  label,
				Message:   "stream ended before response.completed",
				Retryable: true,
			}
		}
		if streamErr != nil {
			send(core.StreamEvent{
				Type: core.StreamError, Err: streamErr,
				Message: buildRespMessage(thinkings, text.String(), calls, stopReason, model),
			})
			return
		}
		msg := buildRespMessage(thinkings, text.String(), calls, stopReason, model)
		msg.Usage = usage
		send(core.StreamEvent{Type: core.StreamDone, Message: msg, Usage: usage})
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // event:/id: lines and ": ping" comments
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var ev respEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			streamErr = &APIError{
				Provider: label, Retryable: false,
				Message: fmt.Sprintf("bad SSE event: %v: %.256s", err, data),
			}
			break
		}
		switch ev.Type {
		case "response.created":
			ensureStart()
		case "response.output_item.added":
			ensureStart()
			applyItem(ev.OutputIndex, ev.Item)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			ensureStart()
			th := thinkingFor(ev.OutputIndex)
			th.text += ev.Delta
			emitThinking(ev.Delta)
		case "response.reasoning_summary_part.done":
			th := thinkingFor(ev.OutputIndex)
			th.text += "\n\n"
			emitThinking("\n\n")
		case "response.output_text.delta", "response.refusal.delta":
			ensureStart()
			text.WriteString(ev.Delta)
			emitText(ev.Delta)
		case "response.function_call_arguments.delta":
			ensureStart()
			c := callFor(ev.OutputIndex)
			c.args.WriteString(ev.Delta)
			emitToolDelta(c, ev.Delta)
		case "response.function_call_arguments.done":
			ensureStart()
			c := callFor(ev.OutputIndex)
			setRespArgs(c, ev.Arguments, emitToolDelta)
		case "response.output_item.done":
			ensureStart()
			applyItem(ev.OutputIndex, ev.Item)
		case "response.completed", "response.incomplete":
			ensureStart()
			terminal = true
			var p respPayload
			if err := json.Unmarshal(ev.Response, &p); err != nil {
				streamErr = &APIError{Provider: label, Retryable: false, Message: "bad terminal response: " + err.Error()}
				break
			}
			if p.Status == "" {
				// Some compatible gateways omit status; the event type is
				// authoritative.
				if ev.Type == "response.incomplete" {
					p.Status = "incomplete"
				} else {
					p.Status = "completed"
				}
			}
			usage = respUsageOf(p.Usage)
			backfillReasoning(&p)
			for i, raw := range p.Output {
				applyItem(i, raw)
			}
			var stopErr error
			stopReason, stopErr = respStopReason(&p)
			if stopErr != nil {
				streamErr = &APIError{Provider: label, Retryable: false, Message: stopErr.Error()}
			}
			if stopReason == core.StopEndTurn && len(calls) > 0 {
				stopReason = core.StopToolUse
			}
		case "response.failed":
			terminal = true
			var p respPayload
			_ = json.Unmarshal(ev.Response, &p)
			msg := "response failed"
			if p.Error != nil {
				msg = p.Error.Code + ": " + p.Error.Message
			}
			streamErr = &APIError{Provider: label, Retryable: respErrorRetryable(p.Error), Message: msg}
		case "error":
			terminal = true
			streamErr = &APIError{
				Provider: label, Retryable: respErrorRetryable(&respError{Code: ev.Code, Message: ev.Message}),
				Message: fmt.Sprintf("%s: %s", ev.Code, ev.Message),
			}
		}
	}
	if err := scanner.Err(); err != nil && streamErr == nil && ctx.Err() == nil {
		streamErr = &APIError{Provider: label, Retryable: true, Message: fmt.Sprintf("stream interrupted: %v", err)}
	}
	if ctx.Err() != nil {
		return
	}
	finishStream()
}

// setRespArgs replaces the accumulated arguments with the authoritative
// full value, emitting any suffix the deltas had not delivered yet.
func setRespArgs(c *respCall, full string, emit func(*respCall, string)) {
	prev := c.args.String()
	if full == "" || full == prev {
		return
	}
	if strings.HasPrefix(full, prev) {
		emit(c, full[len(prev):])
	}
	c.args.Reset()
	c.args.WriteString(full)
}

func respSummaryText(item respItem) string {
	var parts []string
	for _, s := range item.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// respReasoningText prefers the OpenAI summary, then falls back to raw
// reasoning content. vLLM (and some other compatible servers) emit
// reasoning items with an empty summary and the actual chain of thought in
// content entries of type "reasoning_text"; the streaming deltas carry the
// same text, but the terminal output may be the only complete copy.
func respReasoningText(item respItem) string {
	if s := respSummaryText(item); s != "" {
		return s
	}
	var parts []string
	for _, c := range item.Content {
		if c.Type == "reasoning_text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// reasoningItemHasPayload reports whether an output item carries anything
// worth persisting for replay (an id-only "added" item does not).
func reasoningItemHasPayload(item respItem) bool {
	return item.EncryptedContent != "" || len(item.Summary) > 0 || len(item.Content) > 0
}

func respContentText(item respItem) string {
	var b strings.Builder
	for _, c := range item.Content {
		switch c.Type {
		case "output_text":
			b.WriteString(c.Text)
		case "refusal":
			b.WriteString(c.Refusal)
		}
	}
	return b.String()
}

func respUsageOf(u *respUsage) *core.Usage {
	if u == nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return &core.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: total}
}

func respStopReason(p *respPayload) (string, error) {
	switch p.Status {
	case "completed":
		return core.StopEndTurn, nil
	case "incomplete":
		if p.IncompleteDetails != nil && p.IncompleteDetails.Reason == "max_output_tokens" {
			return core.StopLength, nil
		}
		reason := "response incomplete"
		if p.IncompleteDetails != nil && p.IncompleteDetails.Reason != "" {
			reason += ": " + p.IncompleteDetails.Reason
		}
		return core.StopError, errors.New(reason)
	case "failed", "cancelled":
		if p.Error != nil && p.Error.Message != "" {
			return core.StopError, errors.New(p.Error.Message)
		}
		return core.StopError, errors.New("response " + p.Status)
	default:
		return core.StopEndTurn, nil
	}
}

func respErrorRetryable(e *respError) bool {
	if e == nil {
		return false
	}
	code := strings.ToLower(e.Code + " " + e.Message)
	for _, needle := range []string{"rate_limit", "server_error", "overloaded", "timeout", "temporarily"} {
		if strings.Contains(code, needle) {
			return true
		}
	}
	return false
}

func mergeEncryptedContent(raw json.RawMessage, enc string) json.RawMessage {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	if existing, _ := fields["encrypted_content"].(string); existing != "" {
		return raw
	}
	fields["encrypted_content"] = enc
	merged, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return merged
}

// buildRespMessage assembles the neutral assistant message from the
// streamed pieces. Truncated function arguments go through the salvage
// parser; StopReason=length still makes the loop refuse to execute them.
func buildRespMessage(thinkings []*respThinking, text string, calls map[int]*respCall, stop string, model core.Model) *core.Message {
	var blocks []core.Block
	for _, th := range thinkings {
		if th.text == "" && len(th.signature) == 0 {
			continue
		}
		b := core.ThinkingBlock(th.text)
		if len(th.signature) > 0 {
			b.Signature = string(th.signature)
		}
		blocks = append(blocks, b)
	}
	if text != "" {
		blocks = append(blocks, core.TextBlock(text))
	}
	if len(calls) > 0 {
		idxs := make([]int, 0, len(calls))
		for i := range calls {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			c := calls[i]
			args := map[string]any{}
			if c.args.Len() > 0 {
				args, _ = core.ParseSalvage(c.args.String())
			}
			raw, _ := json.Marshal(args)
			b := core.ToolCallBlock(c.callID, c.name, raw)
			b.ItemID = c.itemID
			blocks = append(blocks, b)
		}
	}
	return &core.Message{
		Role: core.RoleAssistant, Content: blocks, Model: model.ID,
		StopReason: stop,
	}
}
