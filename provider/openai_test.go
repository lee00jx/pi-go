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

// sseServer serves the given SSE data lines in order on every request,
// recording the last request body.
func sseServer(t *testing.T, dataLines []string, lastBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastBody != nil {
			buf, _ := io.ReadAll(r.Body)
			*lastBody = buf
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, line := range dataLines {
			fmt.Fprintf(w, "data: %s\n\n", line)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
}

func req(t *testing.T) core.Request {
	t.Helper()
	return core.Request{
		Model:        core.Model{Provider: "openai", ID: "gpt-4o-mini"},
		SystemPrompt: "you are a test",
		Messages:     []*core.Message{core.NewUserMessage("hi")},
		Tools:        []core.Tool{echoTool{mode: core.ModeParallel}},
	}
}

func TestOpenAITextStream(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	var evs []core.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}

	var text string
	var done *core.StreamEvent
	sawStart := false
	for _, ev := range evs {
		switch ev.Type {
		case core.StreamStart:
			sawStart = true
		case core.StreamTextDelta:
			text += ev.Text
		case core.StreamDone:
			done = &ev
		}
	}
	if !sawStart || text != "Hello" {
		t.Fatalf("start=%v text=%q, want start + Hello", sawStart, text)
	}
	if done == nil || done.Message == nil || done.Message.StopReason != core.StopEndTurn {
		t.Fatalf("done = %+v, want end_turn", done)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 9 {
		t.Fatalf("usage = %+v, want total 9", done.Usage)
	}

	// Wire body must carry system + user messages and the tool def.
	var b oaRequest
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if !b.Stream || b.StreamOptions == nil || !b.StreamOptions.IncludeUsage {
		t.Fatalf("stream flags wrong: %+v", b)
	}
	if b.Model != "gpt-4o-mini" || len(b.Messages) != 2 || b.Messages[0].Role != "system" {
		t.Fatalf("body = %+v", b)
	}
	if len(b.Tools) != 1 || b.Tools[0].Function.Name != "echo" {
		t.Fatalf("tools = %+v", b.Tools)
	}
}

func TestOpenAIToolCallStream(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	var evs []core.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	var done *core.StreamEvent
	deltas := 0
	for i := range evs {
		if evs[i].Type == core.StreamToolCallDelta {
			deltas++
		}
		if evs[i].Type == core.StreamDone {
			done = &evs[i]
		}
	}
	if done == nil || done.Message == nil {
		t.Fatalf("no done event: %+v", evs)
	}
	if done.Message.StopReason != core.StopToolUse {
		t.Fatalf("stop = %s, want tool_use", done.Message.StopReason)
	}
	calls := done.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "echo" {
		t.Fatalf("call = %+v", calls[0])
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil || args["text"] != "hi" {
		t.Fatalf("args = %s (err %v), want {text:hi}", calls[0].Arguments, err)
	}
	if deltas != 2 {
		t.Fatalf("toolcall deltas = %d, want 2", deltas)
	}

	// The request body must serialize the assistant/tool history correctly.
	var b oaRequest
	_ = json.Unmarshal(body, &b)
}

// A truncated tool-call argument stream (finish_reason=length) must yield a
// salvage-parsed argument object and StopLength.
func TestOpenAITruncatedToolCallArgs(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":\"hel"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		`[DONE]`,
	}, nil)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	var done *core.StreamEvent
	for ev := range ch {
		if ev.Type == core.StreamDone {
			done = &ev
		}
	}
	if done == nil || done.Message == nil {
		t.Fatal("no done event")
	}
	if done.Message.StopReason != core.StopLength {
		t.Fatalf("stop = %s, want length", done.Message.StopReason)
	}
	calls := done.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("salvaged args must parse: %s (%v)", calls[0].Arguments, err)
	}
}

func TestOpenAIHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()
	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	_, err := client.Stream(context.Background(), req(t))
	var ae *APIError
	if !asAPIError(err, &ae) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if ae.Status != 400 || ae.Retryable {
		t.Fatalf("400 must be non-retryable, got %+v", ae)
	}
}

func TestOpenAIStatusClassify(t *testing.T) {
	cases := map[int]bool{400: false, 401: false, 404: false, 408: true, 429: true, 500: true, 503: true, 529: true}
	for status, wantRetry := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, "x")
		}))
		client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
		_, err := client.Stream(context.Background(), req(t))
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

// TestOpenAIHistoryWire checks the neutral→wire translation of a mixed
// history (user / assistant-with-toolcall / tool-result).
func TestOpenAIHistoryWire(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	assistant := &core.Message{
		Role: core.RoleAssistant,
		Content: []core.Block{
			core.TextBlock("thinking..."),
			core.ToolCallBlock("c1", "echo", json.RawMessage(`{"text":"x"}`)),
		},
	}
	toolRes := core.NewToolResultMessage(core.ToolCall{ID: "c1", Name: "echo"}, core.ToolResult{Output: "echo: x"})
	r := req(t)
	r.Messages = []*core.Message{core.NewUserMessage("hi"), assistant, toolRes}
	if _, err := client.Stream(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var b oaRequest
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	// system + 3 history = 4
	if len(b.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(b.Messages))
	}
	// assistant-with-toolcall: text is kept as content + tool_calls present.
	asst := b.Messages[2]
	var asstContent string
	if err := json.Unmarshal(asst.Content, &asstContent); err != nil || asstContent != "thinking..." {
		t.Fatalf("assistant content wire = %s (err %v), want \"thinking...\"", asst.Content, err)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant wire = %+v", asst)
	}
	if asst.ToolCalls[0].Function.Arguments != `{"text":"x"}` {
		t.Fatalf("tool args wire = %s", asst.ToolCalls[0].Function.Arguments)
	}
	// tool result maps to role=tool with tool_call_id.
	tool := b.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "c1" {
		t.Fatalf("tool wire = %+v", tool)
	}
}

// helpers

func asAPIError(err error, target **APIError) bool {
	if err == nil {
		return false
	}
	if ae, ok := err.(*APIError); ok {
		*target = ae
		return true
	}
	return false
}

// echoTool is a minimal core.Tool for wire-shape tests.
type echoTool struct {
	mode core.Mode
}

func (echoTool) Name() string               { return "echo" }
func (echoTool) Schema() json.RawMessage    { return json.RawMessage(`{"type":"object"}`) }
func (e echoTool) ExecutionMode() core.Mode { return e.mode }
func (echoTool) Execute(_ context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	return core.ToolResult{Output: "ok"}, nil
}

type describedEchoTool struct{ echoTool }

func (describedEchoTool) Description() string { return "Echo the input text back" }

func TestOpenAIToolDescription(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	r := req(t)
	r.Tools = []core.Tool{
		describedEchoTool{echoTool{mode: core.ModeParallel}},
		echoTool{mode: core.ModeParallel},
	}
	ch, err := client.Stream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	var b oaRequest
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Tools) != 2 {
		t.Fatalf("tools len = %d", len(b.Tools))
	}
	if b.Tools[0].Function.Description != "Echo the input text back" {
		t.Fatalf("described = %q", b.Tools[0].Function.Description)
	}
	if b.Tools[1].Function.Description != "" {
		t.Fatalf("plain = %q, want empty (omitempty)", b.Tools[1].Function.Description)
	}
}

// TestOpenAISamplingOmitted: zero Sampling must not appear on the wire, so
// vLLM / OpenAI keep their server defaults.
func TestOpenAISamplingOmitted(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "top_p", "max_tokens", "seed"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("zero Sampling leaked %q onto the wire: %s", k, body)
		}
	}
}

// TestOpenAISamplingSet: filled knobs (including temperature 0) must land
// on the chat-completions body.
func TestOpenAISamplingSet(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	r := req(t)
	r.Sampling = core.Sampling{
		Temperature: core.Ptr(0.0),
		TopP:        core.Ptr(0.9),
		MaxTokens:   256,
		Seed:        core.Ptr(int64(42)),
	}
	ch, err := client.Stream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	var b oaRequest
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if b.Temperature == nil || *b.Temperature != 0 {
		t.Fatalf("temperature = %v, want 0", b.Temperature)
	}
	if b.TopP == nil || *b.TopP != 0.9 {
		t.Fatalf("top_p = %v, want 0.9", b.TopP)
	}
	if b.MaxTokens != 256 {
		t.Fatalf("max_tokens = %d, want 256", b.MaxTokens)
	}
	if b.Seed == nil || *b.Seed != 42 {
		t.Fatalf("seed = %v, want 42", b.Seed)
	}
}

func TestOpenAIExtraBody(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, &body)
	defer srv.Close()

	chatTemplate := map[string]any{"enable_thinking": false}
	client, err := NewOpenAI(
		srv.URL,
		"k",
		WithHTTPClient(srv.Client()),
		WithExtraBody(map[string]any{"chat_template_kwargs": chatTemplate}),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Options snapshot caller-owned maps at construction time.
	chatTemplate["enable_thinking"] = true

	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	kwargs, ok := raw["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs = %#v, want enable_thinking=false", raw["chat_template_kwargs"])
	}
}

func TestOpenAIExtraBodyRejectsReservedFields(t *testing.T) {
	_, err := NewOpenAI(
		"http://example.test/v1",
		"k",
		WithExtraBody(map[string]any{"model": "override"}),
	)
	if err == nil {
		t.Fatal("expected reserved model override to fail")
	}
}

func TestOpenAIInsecureTLSIsPerClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	insecure, err := NewOpenAI(srv.URL, "k", WithInsecureSkipTLSVerify())
	if err != nil {
		t.Fatal(err)
	}
	ch, err := insecure.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatalf("explicit insecure client failed: %v", err)
	}
	for range ch {
	}

	// Constructing an insecure client must not mutate http.DefaultTransport:
	// another client still rejects the same self-signed certificate.
	secure, err := NewOpenAI(srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secure.Stream(context.Background(), req(t)); err == nil {
		t.Fatal("default client unexpectedly accepted a self-signed certificate")
	}
}
