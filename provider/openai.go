package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/lee00jx/pi-go/core"
)

// APIError is a provider HTTP error. Retryable reports whether the retry
// wrapper (WithRetry) should back off and try again: 429 / 5xx / 408 and
// transport errors are retryable; 4xx parameter errors are not
// (DESIGN §3.4).
type APIError struct {
	Provider  string
	Status    int
	Message   string
	Retryable bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
}

// IsRetryable reports whether a stream error may be retried by WithRetry.
func IsRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Retryable
	}
	// Transport-level errors (connection refused, timeout, mid-stream EOF)
	// are retryable per DESIGN §3.4.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// OpenAI is an OpenAI-compatible chat-completions client. One client covers
// OpenAI / DeepSeek / 通义 / vLLM: only baseURL, apiKey and model differ
// (DESIGN §3.1).
type OpenAI struct {
	// BaseURL ends without a trailing slash, e.g. https://api.openai.com/v1.
	BaseURL string
	APIKey  string

	// Provider label used in error messages (defaults to "openai").
	Provider string

	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client

	// extraBody contains immutable, provider-specific top-level request
	// fields captured by WithExtraBody. Library-owned fields cannot be
	// overridden.
	extraBody map[string]json.RawMessage
}

// OpenAIOption configures one OpenAI-compatible client.
type OpenAIOption func(*openAIOptions) error

type openAIOptions struct {
	httpClient            *http.Client
	extraBody             map[string]json.RawMessage
	insecureSkipTLSVerify bool
}

// WithHTTPClient supplies the HTTP client used by this provider instance.
// A nil client is rejected; omit the option to use http.DefaultClient.
func WithHTTPClient(client *http.Client) OpenAIOption {
	return func(cfg *openAIOptions) error {
		if client == nil {
			return errors.New("provider: openai HTTP client must not be nil")
		}
		cfg.httpClient = client
		return nil
	}
}

// WithInsecureSkipTLSVerify disables server-certificate verification for
// this provider instance only. It is intended for explicitly trusted
// development/private endpoints; prefer a custom HTTPClient with the
// private CA installed in production.
func WithInsecureSkipTLSVerify() OpenAIOption {
	return func(cfg *openAIOptions) error {
		cfg.insecureSkipTLSVerify = true
		return nil
	}
}

var openAIReservedBodyFields = map[string]struct{}{
	"model": {}, "stream": {}, "stream_options": {}, "messages": {}, "tools": {},
	"temperature": {}, "top_p": {}, "max_tokens": {}, "seed": {},
}

// WithExtraBody adds provider-specific top-level JSON fields to every
// request made by this client (for example vLLM chat_template_kwargs).
// Fields owned by the library are rejected instead of silently overriding
// the typed request.
func WithExtraBody(fields map[string]any) OpenAIOption {
	return func(cfg *openAIOptions) error {
		if len(fields) == 0 {
			return nil
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("provider: openai encode extra body: %w", err)
		}
		var snapshot map[string]json.RawMessage
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return fmt.Errorf("provider: openai decode extra body: %w", err)
		}
		for key := range snapshot {
			if _, reserved := openAIReservedBodyFields[key]; reserved {
				return fmt.Errorf("provider: openai extra body cannot override %q", key)
			}
		}
		if cfg.extraBody == nil {
			cfg.extraBody = make(map[string]json.RawMessage, len(snapshot))
		}
		for key, value := range snapshot {
			cfg.extraBody[key] = value
		}
		return nil
	}
}

// NewOpenAI validates the minimum configuration.
func NewOpenAI(baseURL, apiKey string, opts ...OpenAIOption) (*OpenAI, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("provider: openai baseURL is required")
	}
	if apiKey == "" {
		return nil, errors.New("provider: openai apiKey is required")
	}
	cfg := openAIOptions{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.insecureSkipTLSVerify {
		client, err := cloneHTTPClientWithInsecureTLS(cfg.httpClient)
		if err != nil {
			return nil, err
		}
		cfg.httpClient = client
	}
	return &OpenAI{
		BaseURL: baseURL, APIKey: apiKey, Provider: "openai",
		HTTPClient: cfg.httpClient, extraBody: cfg.extraBody,
	}, nil
}

func cloneHTTPClientWithInsecureTLS(client *http.Client) (*http.Client, error) {
	if client == nil {
		client = http.DefaultClient
	}
	cloned := *client
	roundTripper := client.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok {
		return nil, errors.New("provider: insecure TLS option requires an *http.Transport")
	}
	transport = transport.Clone()
	tlsConfig := &tls.Config{} //nolint:gosec // verification is explicitly disabled by the caller
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
	}
	tlsConfig.InsecureSkipVerify = true //nolint:gosec // explicit per-client opt-in
	transport.TLSClientConfig = tlsConfig
	cloned.Transport = transport
	return &cloned, nil
}

// wire types (chat completions).

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaToolCallMsg `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type oaToolCallMsg struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function oaFunctionCall `json:"function"`
}

type oaFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaToolDef struct {
	Type     string       `json:"type"`
	Function oaToolSchema `json:"function"`
}

type oaToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaRequest struct {
	Model         string       `json:"model"`
	Stream        bool         `json:"stream"`
	StreamOptions *oaStreamOpt `json:"stream_options,omitempty"`
	Messages      []oaMessage  `json:"messages"`
	Tools         []oaToolDef  `json:"tools,omitempty"`
	Temperature   *float64     `json:"temperature,omitempty"`
	TopP          *float64     `json:"top_p,omitempty"`
	MaxTokens     int          `json:"max_tokens,omitempty"`
	Seed          *int64       `json:"seed,omitempty"`
}

type oaStreamOpt struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaChunk struct {
	// Error carries provider errors delivered inside the stream (some
	// compatible servers do this instead of an HTTP status).
	Error   *oaError   `json:"error,omitempty"`
	Choices []oaChoice `json:"choices,omitempty"`
	Usage   *oaUsage   `json:"usage,omitempty"`
}

type oaError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type oaChoice struct {
	Delta        oaDelta `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type oaDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content"`
	ToolCalls []oaToolCallDel `json:"tool_calls,omitempty"`
	// ReasoningContent is the vLLM / Qwen3 reasoning-parser field.
	// Reasoning is a shorter alias used by some compatible gateways.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"`
}

type oaToolCallDel struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Stream implements core.StreamFn against the chat completions API.
func (o *OpenAI) Stream(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
	if req.Model.ID == "" {
		return nil, errors.New("provider: openai model id is required")
	}
	body, err := o.marshalRequest(oaRequest{
		Model:         req.Model.ID,
		Stream:        true,
		StreamOptions: &oaStreamOpt{IncludeUsage: true},
		Messages:      o.buildMessages(req),
		Tools:         buildToolDefs(req.Tools),
		Temperature:   req.Sampling.Temperature,
		TopP:          req.Sampling.TopP,
		MaxTokens:     req.Sampling.MaxTokens,
		Seed:          req.Sampling.Seed,
	})
	if err != nil {
		return nil, fmt.Errorf("provider: openai encode request: %w", err)
	}

	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	label := o.Provider
	if label == "" {
		label = "openai"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		// Transport error: retryable (WithRetry).
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
	go o.readStream(ctx, resp, req.Model, out)
	return out, nil
}

func (o *OpenAI) marshalRequest(req oaRequest) ([]byte, error) {
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

// tcAccum gathers one streamed tool call's identity and argument fragments
// (keyed by the provider's tool_calls index).
type tcAccum struct {
	id, name string
	args     strings.Builder
}

// readStream consumes the SSE response and emits normalized events.
func (o *OpenAI) readStream(ctx context.Context, resp *http.Response, model core.Model, out chan<- core.StreamEvent) {
	defer resp.Body.Close()
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

	var thinking strings.Builder
	var text strings.Builder
	var split thinkSplit
	calls := map[int]*tcAccum{}
	var usage *core.Usage
	var finish string
	started := false
	var streamErr error

	emitThinking := func(s string) {
		if s == "" {
			return
		}
		thinking.WriteString(s)
		send(core.StreamEvent{Type: core.StreamThinkingDelta, Text: s})
	}
	emitText := func(s string) {
		if s == "" {
			return
		}
		text.WriteString(s)
		send(core.StreamEvent{Type: core.StreamTextDelta, Text: s})
	}
	flushSplit := func() {
		th, tx := split.Finish()
		emitThinking(th)
		emitText(tx)
	}

	finishStream := func() {
		flushSplit()
		if streamErr != nil {
			send(core.StreamEvent{Type: core.StreamError, Err: streamErr, Message: currentMessage(thinking.String(), text.String(), calls, model)})
			return
		}
		msg := assembleMessage(thinking.String(), text.String(), calls, model)
		switch finish {
		case "length":
			msg.StopReason = core.StopLength
		case "tool_calls":
			msg.StopReason = core.StopToolUse
		case "content_filter":
			msg.StopReason = core.StopError
		default:
			msg.StopReason = core.StopEndTurn
		}
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
			continue // comment (": ping"), event:/id: lines are unused here
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk oaChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			streamErr = &APIError{Provider: label, Message: fmt.Sprintf("bad SSE chunk: %v: %.256s", err, data), Retryable: false}
			break
		}
		if chunk.Error != nil {
			streamErr = &APIError{Provider: label, Message: chunk.Error.Message, Retryable: false}
			break
		}
		if chunk.Usage != nil {
			usage = &core.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
		for _, ch := range chunk.Choices {
			if !started {
				started = true
				send(core.StreamEvent{Type: core.StreamStart, Message: &core.Message{Role: core.RoleAssistant, Model: model.ID}})
			}
			if rc := ch.Delta.ReasoningContent; rc != "" {
				emitThinking(rc)
			} else if ch.Delta.Reasoning != "" {
				emitThinking(ch.Delta.Reasoning)
			}
			if ch.Delta.Content != "" {
				th, tx := split.Feed(ch.Delta.Content)
				emitThinking(th)
				emitText(tx)
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc := calls[tc.Index]
				if acc == nil {
					acc = &tcAccum{}
					calls[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name += tc.Function.Name // names can stream in fragments
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					// Stream the raw JSON fragment: the frontend renders it
					// live and the final message carries the salvage-parsed
					// object.
					send(core.StreamEvent{
						Type: core.StreamToolCallDelta,
						Text: tc.Function.Arguments, ToolCallID: acc.id, ToolName: acc.name,
					})
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil && streamErr == nil && ctx.Err() == nil {
		// Mid-stream disconnect: keep the partial, surface as a retryable
		// error (DESIGN §3.4).
		streamErr = &APIError{Provider: label, Message: fmt.Sprintf("stream interrupted: %v", err), Retryable: true}
	}
	finishStream()
	close(out)
}

// buildMessages translates the neutral message model to the wire format.
// Thinking blocks are dropped (no OpenAI-compatible equivalent; they belong
// to Anthropic, DESIGN §3.5).
func (o *OpenAI) buildMessages(req core.Request) []oaMessage {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: jsonString(req.SystemPrompt)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case core.RoleUser:
			msgs = append(msgs, oaMessage{Role: "user", Content: jsonString(m.Text())})
		case core.RoleAssistant:
			var tc []oaToolCallMsg
			var textParts []string
			for _, b := range m.Content {
				switch b.Type {
				case core.BlockText:
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case core.BlockToolCall:
					args := b.Arguments
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					tc = append(tc, oaToolCallMsg{
						ID: b.ToolCallID, Type: "function",
						Function: oaFunctionCall{Name: b.Name, Arguments: string(args)},
					})
				}
			}
			om := oaMessage{Role: "assistant"}
			if len(textParts) > 0 {
				om.Content = jsonString(strings.Join(textParts, ""))
			} else {
				om.Content = json.RawMessage("null")
			}
			om.ToolCalls = tc
			msgs = append(msgs, om)
		case core.RoleTool:
			var textParts []string
			for _, b := range m.Content {
				if b.Type == core.BlockText {
					textParts = append(textParts, b.Text)
				}
			}
			msgs = append(msgs, oaMessage{
				Role: "tool", ToolCallID: m.ToolCallID,
				Content: jsonString(strings.Join(textParts, "")),
			})
		}
	}
	return msgs
}

func buildToolDefs(tools []core.Tool) []oaToolDef {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]oaToolDef, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema()
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		defs = append(defs, oaToolDef{
			Type: "function",
			Function: oaToolSchema{
				Name:        t.Name(),
				Description: core.ToolDescription(t),
				Parameters:  schema,
			},
		})
	}
	return defs
}

// currentMessage builds the partial message for an error mid-stream.
func currentMessage(thinking, text string, calls map[int]*tcAccum, model core.Model) *core.Message {
	return assembleMessage(thinking, text, calls, model)
}

// assembleMessage finalizes the streamed pieces into one assistant message.
// Truncated tool-call arguments are finalized with the salvage parser
// (DESIGN §2.3): they may parse to a best-effort object, but the loop's
// truncation protection (StopReason=length) keeps them from executing.
func assembleMessage(thinking, text string, calls map[int]*tcAccum, model core.Model) *core.Message {
	var blocks []core.Block
	if thinking != "" {
		blocks = append(blocks, core.ThinkingBlock(thinking))
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
			acc := calls[i]
			args := map[string]any{}
			if acc.args.Len() > 0 {
				args, _ = core.ParseSalvage(acc.args.String())
			}
			raw, _ := json.Marshal(args)
			blocks = append(blocks, core.ToolCallBlock(acc.id, acc.name, raw))
		}
	}
	return &core.Message{
		Role: core.RoleAssistant, Content: blocks, Model: model.ID,
		StopReason: core.StopEndTurn,
	}
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
