// Command hello is the minimal integration example (DESIGN §11, phase 0):
// memory store + scripted (faux) provider + one echo tool + the library
// gateway mounted on plain net/http. It proves the "go get + ~100 lines of
// glue" integration story without touching a real LLM.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"log/slog"
	"net/http"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/gateway"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
	"github.com/lee00jx/pi-go/tools"
)

// echoTool is a trivial demo tool proving the tool-call round trip.
type echoTool struct{}

func (echoTool) Name() string { return "echo" }

func (echoTool) Description() string {
	return "Echo the given text back. Demo tool for the hello example."
}

func (echoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
}

func (echoTool) ExecutionMode() core.Mode { return core.ModeSequential }

func (echoTool) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	var args struct{ Text string }
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return core.ToolResult{Output: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	return core.ToolResult{Output: "echo: " + args.Text}, nil
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	store := session.NewMemoryStore()
	faux := provider.NewFaux(
		core.Model{Provider: "faux", ID: "faux-mini", ContextWindow: 128000},
		provider.FauxTurn{
			Text:      "Hello from pi-go! Let me prove tool calls work.",
			ToolCalls: []provider.FauxToolCall{{Name: "echo", Args: json.RawMessage(`{"text":"pi-go"}`)}},
		},
		provider.FauxTurn{
			Text: "The echo tool replied \"echo: pi-go\". That is one full ReAct round — swap the faux provider for a real one to talk to an LLM.",
		},
	)
	reg := tools.NewRegistry(echoTool{})
	runner := &core.Runner{
		Store:        store,
		StreamFn:     faux.Stream,
		Tools:        reg.List(),
		Model:        faux.Model,
		SystemPrompt: "You are a minimal demo assistant.",
		MaxTurns:     8,
		Logger:       slog.Default(),
	}
	gw := gateway.New(store, runner, gateway.WithAnonymousUser("local"))

	log.Printf("pi-go example listening on %s", *addr)
	log.Printf(`
  note: this demo runs without a real auth layer, so the gateway uses
  WithAnonymousUser("local") (dev fallback). Production deployments
  should stamp ContextWithUserID from their JWT middleware instead —
  without an identity the gateway answers 401.
  1. create a session:
     curl -s -X POST %s/api/agent/sessions -d '{}'
  2. send a prompt (replace <id> with the session id from step 1):
     curl -s -X POST %s/api/agent/sessions/<id>/prompts -d '{"text":"hi"}'
  3. watch the event stream (start it before step 2 for the full flow):
     curl -N %s/api/agent/sessions/<id>/events
`, *addr, *addr, *addr)
	log.Fatal(http.ListenAndServe(*addr, gw))
}
