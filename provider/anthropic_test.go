package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lee00jx/pi-go/core"
)

// anSSEFrame is one Anthropic SSE frame: an event: line plus a data: line
// (unlike the OpenAI client, Anthropic tags every frame with its event
// name, so the test server must emit both lines).
type anSSEFrame struct {
	Event string
	Data  string
}

func anSSEServer(t *testing.T, frames []anSSEFrame, lastBody *[]byte, lastHeader *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastBody != nil {
			buf, _ := io.ReadAll(r.Body)
			*lastBody = buf
		}
		if lastHeader != nil {
			*lastHeader = r.Header
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Event, f.Data)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
}

func anReq(t *testing.T) core.Request {
	t.Helper()
	return core.Request{
		Model:        core.Model{Provider: "anthropic", ID: "claude-opus"},
		SystemPrompt: "you are a test",
		Messages:     []*core.Message{core.NewUserMessage("hi")},
		Tools:        []core.Tool{echoTool{mode: core.ModeParallel}},
	}
}

func newAnClient(t *testing.T, srv *httptest.Server) *Anthropic {
	t.Helper()
	c, err := NewAnthropic("k")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()
	return c
}

// collectAn drains a stream into slices the assertions need.
func collectAn(t *testing.T, c *Anthropic, r core.Request) ([]core.StreamEvent, *core.StreamEvent) {
	t.Helper()
	ch, err := c.Stream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var evs []core.StreamEvent
	var done *core.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
		if ev.Type == core.StreamDone {
			done = &evs[len(evs)-1]
		}
	}
	return evs, done
}

func TestAnthropicTextStream(t *testing.T) {
	var body []byte
	var hdr http.Header
	srv := anSSEServer(t, []anSSEFrame{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus","usage":{"input_tokens":25,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":25,"output_tokens":7}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, &body, &hdr)
	defer srv.Close()

	evs, done := collectAn(t, newAnClient(t, srv), anReq(t))

	var text string
	for _, ev := range evs {
		if ev.Type == core.StreamTextDelta {
			text += ev.Text
		}
	}
	if text != "Hello" {
		t.Fatalf("text = %q, want Hello", text)
	}
	if done == nil || done.Message == nil || done.Message.StopReason != core.StopEndTurn {
		t.Fatalf("done = %+v, want end_turn", done)
	}
	// usage = message_start input + message_delta output (the delta's
	// output_tokens overrides the start's).
	if done.Usage == nil || done.Usage.InputTokens != 25 || done.Usage.OutputTokens != 7 ||
		done.Usage.TotalTokens != 32 {
		t.Fatalf("usage = %+v, want in25/out7/total32", done.Usage)
	}

	// Headers: x-api-key + anthropic-version.
	if got := hdr.Get("x-api-key"); got != "k" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := hdr.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}

	// Wire body: max_tokens required (client default), system as a block
	// array, user message as a plain string, tool def with input_schema.
	var b struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if !b.Stream || b.Model != "claude-opus" || b.MaxTokens != 8192 {
		t.Fatalf("body head = model %s max %d stream %v", b.Model, b.MaxTokens, b.Stream)
	}
	if len(b.System) != 1 || b.System[0].Type != "text" || b.System[0].Text != "you are a test" {
		t.Fatalf("system = %+v", b.System)
	}
	if len(b.Messages) != 1 || b.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", b.Messages)
	}
	var userContent string
	if err := json.Unmarshal(b.Messages[0].Content, &userContent); err != nil || userContent != "hi" {
		t.Fatalf("user content wire = %s (err %v), want \"hi\"", b.Messages[0].Content, err)
	}
	if len(b.Tools) != 1 || b.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", b.Tools)
	}
}

func TestAnthropicToolDescription(t *testing.T) {
	var body []byte
	srv := anSSEServer(t, anTextFrames(), &body, nil)
	defer srv.Close()
	r := anReq(t)
	r.Tools = []core.Tool{
		describedEchoTool{echoTool{mode: core.ModeParallel}},
		echoTool{mode: core.ModeParallel},
	}
	collectAn(t, newAnClient(t, srv), r)

	var b struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Tools) != 2 {
		t.Fatalf("tools len = %d", len(b.Tools))
	}
	if b.Tools[0].Description != "Echo the input text back" {
		t.Fatalf("described = %q", b.Tools[0].Description)
	}
	if b.Tools[1].Description != "" {
		t.Fatalf("plain = %q, want omitted", b.Tools[1].Description)
	}
}

func TestAnthropicToolCallStream(t *testing.T) {
	srv := anSSEServer(t, []anSSEFrame{
		{"message_start", `{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Checking."}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" the weather."}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"echo","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"text\":"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"hi\"}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, nil, nil)
	defer srv.Close()

	evs, done := collectAn(t, newAnClient(t, srv), anReq(t))
	if done == nil || done.Message == nil {
		t.Fatalf("no done event: %+v", evs)
	}
	if done.Message.StopReason != core.StopToolUse {
		t.Fatalf("stop = %s, want tool_use", done.Message.StopReason)
	}
	// Block order: text then tool call.
	if got := done.Message.Text(); got != "Checking. the weather." {
		t.Fatalf("text = %q", got)
	}
	calls := done.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "toolu_01" || calls[0].Name != "echo" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil || args["text"] != "hi" {
		t.Fatalf("args = %s (err %v)", calls[0].Arguments, err)
	}
	// input_json_delta fragments stream through as toolcall deltas.
	deltas := 0
	for _, ev := range evs {
		if ev.Type == core.StreamToolCallDelta {
			deltas++
		}
	}
	if deltas != 2 {
		t.Fatalf("toolcall deltas = %d, want 2", deltas)
	}
}

// A max_tokens-truncated tool call must salvage-parse and yield StopLength
// (the loop then refuses to execute it).
func TestAnthropicTruncatedToolCall(t *testing.T) {
	srv := anSSEServer(t, []anSSEFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"echo","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"text\":\"hel"}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":3}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, nil, nil)
	defer srv.Close()

	_, done := collectAn(t, newAnClient(t, srv), anReq(t))
	if done == nil || done.Message == nil {
		t.Fatal("no done event")
	}
	if done.Message.StopReason != core.StopLength {
		t.Fatalf("stop = %s, want length", done.Message.StopReason)
	}
	calls := done.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("salvaged args must parse: %s (%v)", calls[0].Arguments, err)
	}
}

// thinking_delta + signature_delta must land in the assistant message as a
// thinking block carrying the signature (required to round-trip extended
// thinking back to Anthropic).
func TestAnthropicThinking(t *testing.T) {
	srv := anSSEServer(t, []anSSEFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":2,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"Let me think."}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" More."}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Answer"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, nil, nil)
	defer srv.Close()

	_, done := collectAn(t, newAnClient(t, srv), anReq(t))
	if done == nil || done.Message == nil {
		t.Fatal("no done event")
	}
	var think *core.Block
	for i := range done.Message.Content {
		if done.Message.Content[i].Type == core.BlockThinking {
			think = &done.Message.Content[i]
		}
	}
	if think == nil {
		t.Fatalf("no thinking block: %+v", done.Message.Content)
	}
	if think.Thinking != "Let me think. More." || think.Signature != "sig123" {
		t.Fatalf("thinking = %q sig %q", think.Thinking, think.Signature)
	}
	if got := done.Message.Text(); got != "Answer" {
		t.Fatalf("text = %q", got)
	}
}

func TestAnthropicHTTPErrors(t *testing.T) {
	if _, err := NewAnthropic(""); err == nil {
		t.Fatal("empty key must fail")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`)
	}))
	defer srv.Close()
	client := newAnClient(t, srv)
	_, err := client.Stream(context.Background(), anReq(t))
	var ae *APIError
	if !asAPIError(err, &ae) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if ae.Status != 400 || ae.Retryable || ae.Message != "bad model" {
		t.Fatalf("400 = %+v, want non-retryable with the API message", ae)
	}
}

func TestAnthropicStatusClassify(t *testing.T) {
	cases := map[int]bool{400: false, 401: false, 404: false, 408: true, 429: true, 500: true, 503: true, 529: true}
	for status, wantRetry := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"type":"error","error":{"type":"x","message":"x"}}`)
		}))
		_, err := newAnClient(t, srv).Stream(context.Background(), anReq(t))
		var ae *APIError
		if !asAPIError(err, &ae) {
			srv.Close()
			t.Fatalf("status %d: err = %v, want *APIError", status, err)
		}
		if ae.Retryable != wantRetry {
			t.Fatalf("status %d: retryable = %v, want %v", status, ae.Retryable, wantRetry)
		}
		srv.Close()
	}
}

// TestAnthropicHistoryWire is the convertToLlm acceptance: a mixed neutral
// history (user / assistant-with-toolcall / consecutive tool results) must
// serialize to Anthropic shapes — tool results merged into ONE user message
// of tool_result blocks, thinking kept only with its signature, and a
// signature-less thinking downgraded to text.
func TestAnthropicHistoryWire(t *testing.T) {
	var body []byte
	srv := anSSEServer(t, []anSSEFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, &body, nil)
	defer srv.Close()

	assistant := &core.Message{
		Role: core.RoleAssistant,
		Content: []core.Block{
			{Type: core.BlockThinking, Thinking: "reasoning", Signature: "sig99"},
			core.TextBlock("I'll echo"),
			core.ToolCallBlock("c1", "echo", json.RawMessage(`{"text":"x"}`)),
		},
	}
	res1 := core.NewToolResultMessage(core.ToolCall{ID: "c1", Name: "echo"}, core.ToolResult{Output: "echo: x"})
	res2 := core.NewToolResultMessage(core.ToolCall{ID: "c2", Name: "echo"}, core.ToolResult{Output: "echo: y", IsError: true})
	assistant2 := &core.Message{
		Role: core.RoleAssistant,
		Content: []core.Block{
			{Type: core.BlockThinking, Thinking: "no sig reasoning"}, // downgrades to text
			core.TextBlock("done"),
		},
	}
	r := anReq(t)
	r.Messages = []*core.Message{core.NewUserMessage("hi"), assistant, res1, res2, assistant2}
	if _, err := newAnClient(t, srv).Stream(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	var b struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	// user, assistant, merged tool results, assistant2 = 4
	if len(b.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %s", len(b.Messages), body)
	}

	// assistant: [thinking(with sig), text, tool_use] in order.
	var asstBlocks []map[string]any
	if err := json.Unmarshal(b.Messages[1].Content, &asstBlocks); err != nil {
		t.Fatalf("assistant content is not an array: %s", b.Messages[1].Content)
	}
	if len(asstBlocks) != 3 || asstBlocks[0]["type"] != "thinking" || asstBlocks[1]["type"] != "text" ||
		asstBlocks[2]["type"] != "tool_use" {
		t.Fatalf("assistant blocks = %v", asstBlocks)
	}
	if asstBlocks[0]["signature"] != "sig99" || asstBlocks[0]["thinking"] != "reasoning" {
		t.Fatalf("thinking block = %v", asstBlocks[0])
	}
	toolUse := asstBlocks[2]
	if toolUse["id"] != "c1" || toolUse["name"] != "echo" {
		t.Fatalf("tool_use = %v", toolUse)
	}

	// The two consecutive tool results MUST merge into one user message
	// (the API rejects user/assistant/user runs that break role alternation
	// at the assistant-tool boundary — pi merges the same way).
	if b.Messages[2].Role != "user" {
		t.Fatalf("merged results role = %s", b.Messages[2].Role)
	}
	var results []map[string]any
	if err := json.Unmarshal(b.Messages[2].Content, &results); err != nil {
		t.Fatalf("merged content is not an array: %s", b.Messages[2].Content)
	}
	if len(results) != 2 || results[0]["type"] != "tool_result" || results[1]["type"] != "tool_result" {
		t.Fatalf("merged results = %v", results)
	}
	if results[0]["tool_use_id"] != "c1" || results[0]["content"] != "echo: x" || results[0]["is_error"] != false {
		t.Fatalf("result1 = %v", results[0])
	}
	if results[1]["tool_use_id"] != "c2" || results[1]["is_error"] != true {
		t.Fatalf("result2 = %v", results[1])
	}

	// assistant2: the signature-less thinking became a text block.
	var asst2 []map[string]any
	if err := json.Unmarshal(b.Messages[3].Content, &asst2); err != nil {
		t.Fatalf("assistant2 content is not an array: %s", b.Messages[3].Content)
	}
	if len(asst2) != 2 || asst2[0]["type"] != "text" || asst2[0]["text"] != "no sig reasoning" ||
		asst2[1]["type"] != "text" || asst2[1]["text"] != "done" {
		t.Fatalf("assistant2 blocks = %v", asst2)
	}
}

func anTextFrames() []anSSEFrame {
	return []anSSEFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
}

func TestAnthropicSamplingOmitted(t *testing.T) {
	var body []byte
	srv := anSSEServer(t, anTextFrames(), &body, nil)
	defer srv.Close()
	collectAn(t, newAnClient(t, srv), anReq(t))
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["max_tokens"] != float64(8192) {
		t.Fatalf("max_tokens = %v, want the Anthropic-required default 8192", raw["max_tokens"])
	}
	for _, k := range []string{"temperature", "top_p", "seed"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("zero Sampling leaked %q onto the Anthropic wire: %s", k, body)
		}
	}
}

func TestAnthropicSamplingSet(t *testing.T) {
	var body []byte
	srv := anSSEServer(t, anTextFrames(), &body, nil)
	defer srv.Close()
	r := anReq(t)
	r.Sampling = core.Sampling{
		Temperature: core.Ptr(0.2),
		TopP:        core.Ptr(0.8),
		MaxTokens:   1024,
		Seed:        core.Ptr(int64(7)), // Anthropic has no seed; must stay off the wire
	}
	collectAn(t, newAnClient(t, srv), r)
	var b anRequest
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if b.Temperature == nil || *b.Temperature != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", b.Temperature)
	}
	if b.TopP == nil || *b.TopP != 0.8 {
		t.Fatalf("top_p = %v, want 0.8", b.TopP)
	}
	if b.MaxTokens != 1024 {
		t.Fatalf("max_tokens = %d, want 1024", b.MaxTokens)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["seed"]; ok {
		t.Fatalf("Anthropic wire must not carry seed: %s", body)
	}
}
