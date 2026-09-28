# pi-go

A reusable Go agent library: ReAct loop, SSE event stream, two-level
context compaction. Designed to drop into a backend (Gin or plain
`net/http`) so the integrator owns tools and prompts; the library owns
the loop.

Module path is `github.com/lee00jx/pi-go`.
Design: [`DESIGN.md`](DESIGN.md). Project memory: [`.pmemory/INDEX.md`](.pmemory/INDEX.md).

## What it does

- ReAct loop with streaming events (`text` / `thinking` / `tool_call`)
- Custom tools via `core.Tool` (your business APIs, plus built-in
  `bash` / `read` / `write` / `edit` / `recall_event`)
- OpenAI-compatible + Anthropic providers, mid-run model switch
- Dangerous-op confirmation (`BeforeToolCall` → SSE pause →
  `POST .../confirmations`)
- Append-only session store (in-memory or official GORM adapter)
- Disconnect-safe SSE replay, graceful shutdown + resume
- L1 tool-result eviction + L2 LLM summary, token budget, rate limit,
  request_id idempotency

## What it does not do

These were in the original DESIGN draft and were **deliberately dropped**:

- **sql tool** — pi has none; do not add one in the library
- **path sandbox / gitignore on fs** — 1:1 with pi; gate dangerous
  paths in `BeforeToolCall`
- **Casbin / JWT inside the library** — the Go backend authenticates
  and authorizes; stamp `ContextWithUserID` and optionally
  `WithAuthorizer`. Tool-level Casbin stays in `BeforeToolCall`
- **Viper loader (`FromViper`)** — fill `Runner` fields yourself
- TUI, CLI, remote session protocol, TypeScript extensions

## Requirements

Go 1.24+. SQLite persistence uses `mattn/go-sqlite3` (CGO).

## Add it to a backend

```
go get github.com/lee00jx/pi-go@latest
```

To develop against a local checkout, add
`replace github.com/lee00jx/pi-go => /path/to/pi-go` to your `go.mod`.

Minimal glue (custom tool + system prompt + gateway):

```go
type queryOrderTool struct{}

func (queryOrderTool) Name() string { return "query_order" }
func (queryOrderTool) Description() string {
    return "Look up an order by id. Returns payment status. Call this when the user asks about an order."
}
func (queryOrderTool) Schema() json.RawMessage {
    return json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string","description":"order id"}},"required":["order_id"]}`)
}
func (queryOrderTool) ExecutionMode() core.Mode { return core.ModeParallel }
func (queryOrderTool) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
    var args struct {
        OrderID string `json:"order_id"`
    }
    if err := json.Unmarshal(call.Arguments, &args); err != nil || args.OrderID == "" {
        return core.ToolResult{Output: "missing order_id", IsError: true}, nil
    }
    // call your own DB / HTTP service
    return core.ToolResult{Output: "order " + args.OrderID + ": paid"}, nil
}

store := session.NewMemoryStore()
oa, _ := provider.NewOpenAI(os.Getenv("PI_BASE_URL"), os.Getenv("PI_API_KEY"))
stream := provider.WithRetry(oa.Stream, nil)
model := core.Model{Provider: "openai", ID: "gpt-4o-mini", ContextWindow: 128000}

runner := &core.Runner{
    Store:        store,
    StreamFn:     stream,
    Tools:        tools.NewRegistry(queryOrderTool{}).List(),
    Model:        model,
    SystemPrompt: "You are an order assistant. Use query_order to look up orders.",
    MaxTurns:     8,
    // Sampling is optional. Leave it zero to omit temperature / top_p /
    // max_tokens / seed and let vLLM (or OpenAI / Anthropic) keep its own
    // defaults. Fill only the knobs you want to override; temperature 0
    // is a real value, so use core.Ptr:
    // Sampling: core.Sampling{Temperature: core.Ptr(0.2), MaxTokens: 2048},
}
gw := gateway.New(store, runner)
// Multiple agent types on one gateway:
// gw := gateway.New(store, calc,
//     gateway.WithAgentRunner("calc", calc),
//     gateway.WithAgentRunner("compare", compare),
// )
// Create then requires {"agent":"calc"|"compare"}; unknown types on
// existing sessions return 503.

// Gin: one catch-all mounts every agent endpoint. The wildcard MUST be named.
r.Any("/api/agent/*catch", gin.WrapH(gw))
```

OpenAI-compatible endpoints accept opt-in provider fields without widening
the core request contract. For example, a vLLM/Qwen deployment can disable
thinking:

```go
oa, err := provider.NewOpenAI(
    baseURL,
    apiKey,
    provider.WithExtraBody(map[string]any{
        "chat_template_kwargs": map[string]any{"enable_thinking": false},
    }),
)
```

`WithExtraBody` rejects library-owned keys such as `model`, `messages`,
`tools`, and sampling fields. TLS certificates are verified by default.
For an explicitly trusted development/private endpoint only,
`WithInsecureSkipTLSVerify()` disables verification on that provider client
without mutating Go's global transport. In production, prefer
`WithHTTPClient` configured with the private CA.

Tool failures must return `IsError: true` (never panic, never abort the
loop). Dangerous tools stay dumb; confirmation is a hook:

```go
Hooks: core.Hooks{
    BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
        if call.Name == "refund" {
            return core.Decision{Confirm: true, Reason: "refund is irreversible"}, nil
        }
        return core.Decision{}, nil
    },
},
```

Custom model-output protocols are optional. A project implements
`output.Processor`; pi-go handles event lifecycle and runs project-defined
validation/sanitizing `output.Stage`s in order:

```go
rawEvents, _ := runner.Run(ctx, sessionID, prompts)
events := output.Transform(
    ctx,
    rawEvents,
    output.WithProcessor(func() output.Processor {
        return NewProjectProcessor()
    }),
    output.WithStages(output.StageFunc(ValidateAndSanitize)),
)
```

Without `WithProcessor`, `output.Transform` returns the original channel and
starts no goroutine. The package does not contain a tag, table, chart, or
JSON-repair implementation. Those rules belong to the integrating project.
When using the built-in gateway, wrap the same transform with
`gateway.WithEventMiddleware`; processed events are then persisted and
replayed like every other `AgentEvent`.

Persistence: pass `adapter/gorm.New(db)` after `Migrate`. For SQLite set
`db.DB().SetMaxOpenConns(1)` plus WAL — two goroutines write the same
file (core messages + gateway ui_events) and a multi-connection pool
returns `database is locked`.

Full wiring (providers, compaction, rate limit, SIGTERM resume):
[`example/gin-integration/main.go`](example/gin-integration/main.go).
Smallest possible: [`example/hello/main.go`](example/hello/main.go).

## HTTP API

Prefix defaults to `/api/agent`. The gateway is fail-closed: mount it
behind your auth layer and stamp `gateway.ContextWithUserID(ctx, userID)`
— requests without an identity get 401. `gateway.WithAnonymousUser`
restores an open dev mode for local use only.

| Method | Path | Purpose |
|---|---|---|
| POST | `/sessions` | create session (user from auth context; optional `agent`) |
| GET | `/sessions` | list current user's sessions (`?agent=` `?limit=`, default 50, max 200) |
| POST | `/sessions/{id}/prompts` | user prompt (`{"text":"..."}`; optional `request_id`; response includes `prompt_id` / `run_id`) |
| GET | `/sessions/{id}/events` | SSE stream (`Last-Event-ID` / `?after=seq`) |
| GET | `/sessions/{id}/messages` | first-screen history |
| GET | `/sessions/{id}/turns` | per-prompt projection (user + activity + results; results from ui_event only) |
| POST | `/sessions/{id}/stop` | cancel in-flight run |
| POST | `/sessions/{id}/confirmations` | allow / deny a dangerous tool |
| POST | `/sessions/{id}/model` | switch provider/model at next turn |
| POST | `/sessions/{id}/resume` | continue an `interrupted` session |
| GET | `/sessions/{id}/usage` | cumulative tokens + cost |
| GET | `/sessions/{id}/context-stats` | occupancy bar |

Typical client: create session → open SSE → send prompt. Confirmation
events pause the run until `/confirmations`.

To seed a turn without calling the LLM (welcome / host-written summary),
use the in-process API `gw.InjectTurn(ctx, sessionID, gateway.InjectTurnInput{...})`
— it appends user + assistant + an `output` ui_event under one `prompt_id`
so `GET /turns` has a non-empty results area. There is no HTTP route for this
yet (same-process hosts call the method directly).

## Layout

```
core/       loop, events, Tool, hooks, compaction, salvage
provider/   OpenAI-compatible, Anthropic, Faux, retry
session/    Store contract + MemoryStore
tools/      registry + bash/fs
output/     optional output Processor + ordered Stage pipeline
gateway/    stdlib http.Handler (SSE / REST)
adapter/    official GORM Store (optional)
example/    hello (net/http) + gin-integration
```

Core packages never import gin / gorm / zap / viper.

## Develop

```
go test -race ./...
cd example/hello && go run .
cd example/gin-integration && go run .   # no PI_API_KEY → faux provider
```

## Known limits (v1)

- Process-local run lock and rate limiter (not multi-instance)
- SQLite persistence verified; MySQL/Postgres share the GORM adapter
  but DECIMAL/TIMESTAMP differences are untested
- No path sandbox on bash/fs (integrator hook)
- `session` package has no dedicated tests (covered via gateway/gorm)
