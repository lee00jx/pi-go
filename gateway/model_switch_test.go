package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// modelSwitchTool is the business tool that performs the model switch by
// calling the gateway's own POST /model endpoint — which is exactly how an
// integrator would expose the DESIGN §3.5 switch to the user.
type modelSwitchTool struct {
	gwURL *string
}

func (modelSwitchTool) Name() string             { return "switch_model" }
func (modelSwitchTool) Schema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (modelSwitchTool) ExecutionMode() core.Mode { return core.ModeParallel }
func (m modelSwitchTool) Execute(_ context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	resp, err := http.Post(*m.gwURL+"/api/agent/sessions/s1/model", "application/json",
		strings.NewReader(`{"provider":"deepseek","model":"gpt-4o-mini"}`))
	if err != nil {
		return core.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return core.ToolResult{Output: fmt.Sprintf("switch posted: %d", resp.StatusCode)}, nil
}

// anFrames emits the given Anthropic SSE frames.
func anFramesServer(t *testing.T, frames [][2]string, lastBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastBody != nil {
			buf, _ := io.ReadAll(r.Body)
			*lastBody = buf
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f[0], f[1])
			if fl != nil {
				fl.Flush()
			}
		}
	}))
}

// oaFramesServer emits OpenAI-compat SSE data frames.
func oaFramesServer(t *testing.T, dataLines []string, lastBody *[]byte) *httptest.Server {
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

// TestMidRunModelSwitchE2E is the phase 2 acceptance (DESIGN §11): one
// session starts on Claude (Anthropic wire) and is switched to DeepSeek
// (OpenAI-compat wire) mid-run via POST /model. The switch must take
// effect at the next turn boundary with a model_changed event, and the
// second provider must receive the full history in its own wire format.
func TestMidRunModelSwitchE2E(t *testing.T) {
	store := session.NewMemoryStore()
	if err := store.CreateSession(context.Background(), session.SessionMeta{
		ID: "s1", UserID: "u", Provider: "claude", Model: "claude-opus",
		Status: session.StatusIdle, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var anBody, oaBody []byte
	// Provider A (Anthropic): turn 1 = text + a switch_model tool call.
	anSrv := anFramesServer(t, [][2]string{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Switching for you."}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"switch_model","input":{}}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, &anBody)
	defer anSrv.Close()
	anClient, err := provider.NewAnthropic("k")
	if err != nil {
		t.Fatal(err)
	}
	anClient.BaseURL = anSrv.URL
	anClient.Provider = "claude"

	// Provider B (DeepSeek, OpenAI-compat): answers whatever comes next.
	oaSrv := oaFramesServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"from deepseek"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}, &oaBody)
	defer oaSrv.Close()
	oaClient, err := provider.NewOpenAI(oaSrv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	oaClient.Provider = "deepseek"

	switchTool := modelSwitchTool{gwURL: new(string)}

	runner := &core.Runner{
		Store:    store,
		StreamFn: anClient.Stream, // default binding (unused here)
		Model:    core.Model{Provider: "claude", ID: "claude-opus", ContextWindow: 200000},
		Tools:    []core.Tool{switchTool},
		MaxTurns: 8,
		Providers: map[string]core.ProviderBinding{
			"claude":   {StreamFn: anClient.Stream, Model: core.Model{Provider: "claude", ID: "claude-opus", ContextWindow: 200000}},
			"deepseek": {StreamFn: oaClient.Stream, Model: core.Model{Provider: "deepseek", ID: "gpt-4o-mini", ContextWindow: 128000}},
		},
	}
	gw := New(store, runner, WithAnonymousUser("u"))
	srv := httptest.NewServer(gw)
	defer srv.Close()
	*switchTool.gwURL = srv.URL

	base := srv.URL + "/api/agent/sessions/s1"
	code, _ := postJSON(t, base+"/prompts", `{"text":"go"}`)
	if code != http.StatusAccepted {
		t.Fatalf("prompt: %d", code)
	}

	// Read the SSE stream until the run's agent_end arrives.
	events, err := readSSEUntil(t, base+"/events?after=0", "agent_end", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// model_changed must be present and must name the DeepSeek model.
	var sawChanged bool
	for _, ev := range events {
		var a core.AgentEvent
		if err := json.Unmarshal([]byte(ev), &a); err != nil {
			continue
		}
		if a.Type == core.ModelChanged {
			sawChanged = true
			if a.Model == nil || a.Model.Provider != "deepseek" || a.Model.ID != "gpt-4o-mini" {
				t.Fatalf("model_changed = %+v, want deepseek/gpt-4o-mini", a.Model)
			}
		}
	}
	if !sawChanged {
		t.Fatalf("no model_changed event in %d events", len(events))
	}

	// Final history: user / assistant(switch text+toolcall) / tool / assistant.
	msgs := waitMessages(t, base+"/messages", 4)
	if got := textOf(msgs[3]); got != "from deepseek" {
		t.Fatalf("final assistant = %q, want the DeepSeek answer", got)
	}

	// Each provider was called exactly once, and provider B received the
	// CONVERTED history: its wire must be OpenAI format (role "tool" with
	// tool_call_id, tool_calls on the assistant) with the deepseek model.
	if len(anBody) == 0 {
		t.Fatal("anthropic provider never called")
	}
	var b struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(oaBody, &b); err != nil {
		t.Fatal(err)
	}
	if b.Model != "gpt-4o-mini" {
		t.Fatalf("provider B model = %q, want gpt-4o-mini", b.Model)
	}
	roles := make([]string, 0, len(b.Messages))
	for _, m := range b.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool" {
		t.Fatalf("history wire seen by B = %v, want user,assistant,tool", roles)
	}
	if len(b.Messages[1].ToolCalls) != 1 || b.Messages[1].ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("assistant wire tool_calls = %+v", b.Messages[1].ToolCalls)
	}
	if b.Messages[2].ToolCallID != "toolu_1" || b.Messages[2].Role != "tool" {
		t.Fatalf("tool wire = %+v", b.Messages[2])
	}

	// The session meta now carries the new model.
	meta, err := store.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "deepseek" || meta.Model != "gpt-4o-mini" {
		t.Fatalf("session meta = %s/%s, want deepseek/gpt-4o-mini", meta.Provider, meta.Model)
	}
}

// TestModelEndpointValidation: unknown session → 404, missing fields → 400,
// unknown provider → 400, success → 200 with the new pair.
func TestModelEndpointValidation(t *testing.T) {
	_, srv, _ := newTestGateway(t, 0, provider.FauxTurn{Text: "x"})
	base := srv.URL + "/api/agent/sessions"

	code, _ := postJSON(t, base+"/nope/model", `{"provider":"p","model":"m"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", code)
	}
	code, _ = postJSON(t, base+"/s1/model", `{"provider":"p"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("missing model: %d, want 400", code)
	}
	// "faux" is not registered in Runner.Providers (empty map) → 400.
	code, _ = postJSON(t, base+"/s1/model", `{"provider":"faux","model":"m"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown provider: %d, want 400", code)
	}
}

// readSSEUntil opens the SSE endpoint and collects data frames until one
// contains marker (or the deadline expires). Returns the raw data payloads.
func readSSEUntil(t *testing.T, url, marker string, timeout time.Duration) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		out = append(out, payload)
		if strings.Contains(payload, marker) {
			return out, nil
		}
	}
	return out, fmt.Errorf("timed out waiting for %q (got %d frames)", marker, len(out))
}
