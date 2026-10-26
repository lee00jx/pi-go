package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/core"
)

func respReq() core.Request {
	return core.Request{
		Model:        core.Model{Provider: "openai", ID: "gpt-5-mini"},
		SystemPrompt: "you are a test",
		Messages:     []*core.Message{core.NewUserMessage("hi")},
		Tools:        []core.Tool{echoTool{mode: core.ModeParallel}},
	}
}

func TestOpenAIResponsesTextAndReasoning(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Let me "}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"think."}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc-1","summary":[{"type":"summary_text","text":"Let me think."}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"Hel"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"lo"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
	}, &body)
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client(), ReasoningSummary: "auto"}
	r := respReq()
	r.ThinkingLevel = core.ThinkingHigh
	ch, err := client.Stream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var evs []core.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}

	var thinking, text string
	started := 0
	var done *core.StreamEvent
	for i := range evs {
		switch evs[i].Type {
		case core.StreamStart:
			started++
		case core.StreamThinkingDelta:
			thinking += evs[i].Text
		case core.StreamTextDelta:
			text += evs[i].Text
		case core.StreamDone:
			done = &evs[i]
		case core.StreamError:
			t.Fatalf("unexpected stream error: %v", evs[i].Err)
		}
	}
	if started != 1 || text != "Hello" {
		t.Fatalf("start=%d text=%q, want one start + Hello", started, text)
	}
	if !strings.Contains(thinking, "Let me think.") {
		t.Fatalf("thinking deltas = %q", thinking)
	}
	if done == nil || done.Message == nil {
		t.Fatal("no done message")
	}
	if done.Message.StopReason != core.StopEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", done.Message.StopReason)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 10 || done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", done.Usage)
	}
	if len(done.Message.Content) != 2 || done.Message.Content[0].Type != core.BlockThinking || done.Message.Content[1].Type != core.BlockText {
		t.Fatalf("content = %+v, want thinking + text", done.Message.Content)
	}
	th := done.Message.Content[0]
	if th.Thinking != "Let me think." {
		t.Fatalf("thinking = %q", th.Thinking)
	}
	var sig map[string]any
	if err := json.Unmarshal([]byte(th.Signature), &sig); err != nil {
		t.Fatalf("signature is not JSON: %v (%s)", err, th.Signature)
	}
	if sig["encrypted_content"] != "enc-1" {
		t.Fatalf("signature encrypted_content = %v, want enc-1", sig["encrypted_content"])
	}

	// Wire request: stateless Responses request with reasoning effort,
	// encrypted reasoning, and flattened function tools.
	var got respRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Stream || got.Store {
		t.Fatalf("stream/store = %v/%v, want true/false", got.Stream, got.Store)
	}
	if got.Model != "gpt-5-mini" || len(got.Input) != 2 {
		t.Fatalf("model/input = %q/%d, want gpt-5-mini/2", got.Model, len(got.Input))
	}
	if got.Reasoning == nil || got.Reasoning.Effort != "high" || got.Reasoning.Summary != "auto" {
		t.Fatalf("reasoning = %+v", got.Reasoning)
	}
	if len(got.Include) != 1 || got.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v", got.Include)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "echo" || got.Tools[0].Type != "function" {
		t.Fatalf("tools = %+v", got.Tools)
	}
}

func TestOpenAIResponsesFunctionCall(t *testing.T) {
	var body []byte
	srv := sseServer(t, []string{
		`{"type":"response.created","response":{"id":"resp_2"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"echo","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"text\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"hi\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"text\":\"hi\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"echo","arguments":"{\"text\":\"hi\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
	}, &body)
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), respReq())
	if err != nil {
		t.Fatal(err)
	}
	var deltas string
	var done *core.StreamEvent
	for ev := range ch {
		switch ev.Type {
		case core.StreamToolCallDelta:
			deltas += ev.Text
		case core.StreamDone:
			e := ev
			done = &e
		}
	}
	if deltas != `{"text":"hi"}` {
		t.Fatalf("tool deltas = %q", deltas)
	}
	if done == nil || done.Message == nil || done.Message.StopReason != core.StopToolUse {
		t.Fatalf("done = %+v, want tool_use", done)
	}
	calls := done.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Name != "echo" {
		t.Fatalf("calls = %+v", calls)
	}
	if string(calls[0].Arguments) != `{"text":"hi"}` {
		t.Fatalf("args = %s", calls[0].Arguments)
	}
	if done.Message.Content[0].ItemID != "fc_1" {
		t.Fatalf("item id = %q, want fc_1", done.Message.Content[0].ItemID)
	}
}

func TestOpenAIResponsesReplayInput(t *testing.T) {
	sig := `{"type":"reasoning","id":"rs_1","encrypted_content":"enc-1","summary":[]}`
	assistant := &core.Message{
		Role: core.RoleAssistant,
		Content: []core.Block{
			{Type: core.BlockThinking, Thinking: "why", Signature: sig},
			core.TextBlock("answer"),
			{Type: core.BlockToolCall, ToolCallID: "call_1", ItemID: "fc_1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)},
		},
	}
	tool := &core.Message{
		Role: core.RoleTool, ToolCallID: "call_1", ToolName: "echo",
		Content: []core.Block{core.TextBlock("ok")},
	}
	r := respReq()
	r.Messages = []*core.Message{core.NewUserMessage("hi"), assistant, tool}

	raw, err := json.Marshal(buildRespInput(r))
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, it := range items {
		typ, _ := it["type"].(string)
		if typ == "" {
			typ, _ = it["role"].(string)
		}
		types = append(types, typ)
	}
	want := []string{"system", "user", "reasoning", "message", "function_call", "function_call_output"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("input types = %v, want %v (%s)", types, want, raw)
	}
	if items[2]["encrypted_content"] != "enc-1" {
		t.Fatalf("reasoning item = %v", items[2])
	}
	if items[4]["id"] != "fc_1" || items[4]["call_id"] != "call_1" {
		t.Fatalf("function_call item = %v", items[4])
	}
	if items[5]["call_id"] != "call_1" || items[5]["output"] != "ok" {
		t.Fatalf("function_call_output item = %v", items[5])
	}
}

func TestOpenAIResponsesSkipsReasoningOnlyMessage(t *testing.T) {
	assistant := &core.Message{
		Role: core.RoleAssistant,
		Content: []core.Block{{
			Type:      core.BlockThinking,
			Thinking:  "aborted reasoning",
			Signature: `{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}`,
		}},
	}
	r := respReq()
	r.Messages = []*core.Message{core.NewUserMessage("hi"), assistant}
	raw, err := json.Marshal(buildRespInput(r))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"reasoning"`) {
		t.Fatalf("orphan reasoning item was replayed: %s", raw)
	}
}

func TestOpenAIResponsesReasoningConfig(t *testing.T) {
	client := &OpenAIResponses{ReasoningSummary: "auto"}
	off := client.buildRequest(func() core.Request {
		r := respReq()
		r.ThinkingLevel = core.ThinkingOff
		return r
	}())
	if off.Reasoning == nil || off.Reasoning.Effort != "none" || len(off.Include) != 0 {
		t.Fatalf("off reasoning = %+v include=%v", off.Reasoning, off.Include)
	}

	unset := client.buildRequest(respReq())
	if unset.Reasoning != nil || len(unset.Include) != 0 {
		t.Fatalf("unset reasoning = %+v include=%v", unset.Reasoning, unset.Include)
	}

	r := respReq()
	r.Sampling.MaxTokens = 5
	if got := client.buildRequest(r).MaxOutputTokens; got != 16 {
		t.Fatalf("max_output_tokens = %d, want clamp to 16", got)
	}
}

func TestOpenAIResponsesIncompleteMaxOutput(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.created","response":{"id":"resp_3"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		`{"type":"response.incomplete","response":{"id":"resp_3","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	}, nil)
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), respReq())
	if err != nil {
		t.Fatal(err)
	}
	var done *core.StreamEvent
	for ev := range ch {
		if ev.Type == core.StreamDone {
			e := ev
			done = &e
		}
	}
	if done == nil || done.Message == nil || done.Message.StopReason != core.StopLength {
		t.Fatalf("done = %+v, want length", done)
	}
}

func TestOpenAIResponsesFailed(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.created","response":{"id":"resp_4"}}`,
		`{"type":"response.failed","response":{"id":"resp_4","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
	}, nil)
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), respReq())
	if err != nil {
		t.Fatal(err)
	}
	var streamErr *core.StreamEvent
	for ev := range ch {
		if ev.Type == core.StreamError {
			e := ev
			streamErr = &e
		}
	}
	if streamErr == nil || streamErr.Err == nil || !IsRetryable(streamErr.Err) {
		t.Fatalf("stream error = %+v, want retryable server_error", streamErr)
	}
	if !strings.Contains(streamErr.Err.Error(), "boom") {
		t.Fatalf("error = %v, want boom", streamErr.Err)
	}
}

func TestOpenAIResponsesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	_, err := client.Stream(context.Background(), respReq())
	if err == nil {
		t.Fatal("want HTTP error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || IsRetryable(err) {
		t.Fatalf("err = %v, want non-retryable 401", err)
	}
}

func TestOpenAIResponsesBackfillsEncryptedReasoning(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.created","response":{"id":"resp_5"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_5","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"late-enc","summary":[{"type":"summary_text","text":"thinking"}]}]}}`,
	}, nil)
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	r := respReq()
	r.ThinkingLevel = core.ThinkingLow
	ch, err := client.Stream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var done *core.StreamEvent
	for ev := range ch {
		if ev.Type == core.StreamDone {
			e := ev
			done = &e
		}
	}
	if done == nil || done.Message == nil || len(done.Message.Content) == 0 {
		t.Fatal("no done message")
	}
	th := done.Message.Content[0]
	if !strings.Contains(th.Signature, "late-enc") {
		t.Fatalf("signature = %s, want late-enc backfilled", th.Signature)
	}
}

// vLLM emits raw reasoning via response.reasoning_text.delta and stores it
// in the reasoning item's content (type "reasoning_text"), with summary
// left empty. The client must show that text without an OpenAI summary.
func TestOpenAIResponsesReasoningTextContent(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.created","response":{"id":"resp_6"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"raw "}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"cot"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"raw cot"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_6","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"raw cot"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
	}, nil)
	defer srv.Close()

	client := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	r := respReq()
	r.ThinkingLevel = core.ThinkingHigh
	ch, err := client.Stream(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var thinking string
	var done *core.StreamEvent
	for ev := range ch {
		switch ev.Type {
		case core.StreamThinkingDelta:
			thinking += ev.Text
		case core.StreamDone:
			e := ev
			done = &e
		case core.StreamError:
			t.Fatalf("unexpected stream error: %v", ev.Err)
		}
	}
	if thinking != "raw cot" {
		t.Fatalf("thinking deltas = %q, want raw cot", thinking)
	}
	if done == nil || done.Message == nil || len(done.Message.Content) == 0 {
		t.Fatal("no done message")
	}
	th := done.Message.Content[0]
	if th.Type != core.BlockThinking || th.Thinking != "raw cot" {
		t.Fatalf("thinking block = %+v, want raw cot", th)
	}
	if !strings.Contains(th.Signature, `"reasoning_text"`) {
		t.Fatalf("signature = %s, want raw reasoning item", th.Signature)
	}
}
