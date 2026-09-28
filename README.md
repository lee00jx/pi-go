# pi-go

[中文](README.zh-CN.md)

pi-go is a Go library for embedding an LLM agent in your backend. You supply
the tools and the system prompt; pi-go runs the conversation loop (model →
tool calls → model …), streams events to the browser over SSE, and stores the
session history.

It is a library, not a service: it compiles into your binary and mounts as an
`http.Handler` on Gin or plain `net/http`.

## Install

```
go get github.com/lee00jx/pi-go@latest
```

Requires Go 1.24+.

## Quick start

Three steps: write a tool, build a `Runner`, mount the gateway.

```go
// 1. A tool is your business API, described so the model knows when to call it.
type queryOrder struct{}

func (queryOrder) Name() string        { return "query_order" }
func (queryOrder) Description() string { return "Look up an order by id and return its payment status." }
func (queryOrder) Schema() json.RawMessage {
    return json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`)
}
func (queryOrder) ExecutionMode() core.Mode { return core.ModeParallel }
func (queryOrder) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
    var args struct{ OrderID string `json:"order_id"` }
    if json.Unmarshal(call.Arguments, &args) != nil || args.OrderID == "" {
        return core.ToolResult{Output: "missing order_id", IsError: true}, nil
    }
    return core.ToolResult{Output: "order " + args.OrderID + ": paid"}, nil
}

// 2. A Runner holds the model, tools and prompt.
store := session.NewMemoryStore()
oa, _ := provider.NewOpenAI(os.Getenv("PI_BASE_URL"), os.Getenv("PI_API_KEY"))
runner := &core.Runner{
    Store:        store,
    StreamFn:     provider.WithRetry(oa.Stream, nil),
    Model:        core.Model{Provider: "openai", ID: "gpt-4o-mini", ContextWindow: 128000},
    Tools:        tools.NewRegistry(queryOrder{}).List(),
    SystemPrompt: "You are an order assistant.",
    MaxTurns:     8,
}

// 3. Mount the gateway behind your auth. It rejects requests without a user (401).
gw := gateway.New(store, runner)
protected := r.Group("/", func(c *gin.Context) {
    uid := "..." // from your JWT / session
    c.Request = c.Request.WithContext(gateway.ContextWithUserID(c.Request.Context(), uid))
})
protected.Any("/api/agent/*catch", gin.WrapH(gw))
```

Runnable versions: [`example/hello`](example/hello/main.go) (smallest) and
[`example/gin-integration`](example/gin-integration/main.go) (full setup:
real providers, SQLite, compaction, graceful shutdown). Without
`PI_API_KEY` the Gin example uses a fake model, so it runs offline.

## HTTP API

All paths are under the prefix `/api/agent` (configurable, see below). The
caller must be authenticated as described in step 3.

| Method | Path | Purpose |
|---|---|---|
| POST | `/sessions` | Create a session (optional `{"agent":"..."}`) |
| GET | `/sessions` | List the current user's sessions (`?agent=`, `?limit=`) |
| POST | `/sessions/{id}/prompts` | Send a message: `{"text":"..."}` |
| GET | `/sessions/{id}/events` | SSE event stream; reconnect with `Last-Event-ID` |
| GET | `/sessions/{id}/messages` | Message history |
| GET | `/sessions/{id}/turns` | History grouped per user message |
| POST | `/sessions/{id}/stop` | Cancel the current run |
| POST | `/sessions/{id}/confirmations` | Allow or deny a tool waiting for confirmation |
| POST | `/sessions/{id}/model` | Switch model from the next turn |
| POST | `/sessions/{id}/resume` | Continue a run interrupted by a restart |
| GET | `/sessions/{id}/usage` | Token usage and cost |
| GET | `/sessions/{id}/context-stats` | How full the context window is |

A typical client creates a session, opens the event stream, then sends
messages.

## Common configuration

**Route prefix.** `gateway.WithBasePath("/api/v1/agent")`. The gateway matches
the full path itself, so this must equal the path your router forwards;
otherwise requests reach the gateway and get 404.

**Several agents on one gateway.** Register each runner by name; clients then
pass `{"agent":"calc"}` when creating a session.

```go
gw := gateway.New(store, calcRunner,
    gateway.WithAgentRunner("calc", calcRunner),
    gateway.WithAgentRunner("compare", compareRunner),
)
```

**Confirm dangerous tools.** Keep the tool simple and ask in a hook; the run
pauses until the client posts to `/confirmations`.

```go
runner.Hooks = core.Hooks{
    BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
        if call.Name == "refund" {
            return core.Decision{Confirm: true, Reason: "refund is irreversible"}, nil
        }
        return core.Decision{}, nil
    },
}
```

**Persist to a database.** Use the GORM adapter with any GORM driver; pi-go
itself does not pick one. With SQLite, allow a single connection and enable
WAL, otherwise concurrent writes fail with `database is locked`.

```go
import gormstore "github.com/lee00jx/pi-go/adapter/gorm"

s := gormstore.New(db)
if err := s.Migrate(ctx); err != nil { ... }
gw := gateway.New(s, runner)
```

**Post-process model output.** If your model writes a custom format (tags,
tables, charts), implement `output.Processor` to split it into chunks and
`output.Stage`s to validate or fix them. Hook it into the gateway with
`gateway.WithEventMiddleware`. pi-go ships no format of its own.

```go
gateway.WithEventMiddleware(func(ctx context.Context, in <-chan core.AgentEvent) <-chan core.AgentEvent {
    return output.Transform(ctx, in,
        output.WithProcessor(NewMyProcessor),
        output.WithStages(MyValidateStage()),
    )
})
```

**Other options.** `WithRateLimit` (concurrent runs and prompts per minute
per user), `WithAuthorizer` (extra access check per session),
`gw.InjectTurn` (write a welcome or summary turn without calling the model),
`provider.WithExtraBody` (vendor-specific request fields, e.g. turning off
Qwen thinking), `core.Runner.Sampling` (temperature etc.; leave empty to use
the server defaults). See the Go doc comments for details.

## Out of scope

pi-go deliberately leaves these to your backend: authentication and
permissions (JWT, Casbin), SQL tools, file-system sandboxing (use
`BeforeToolCall`), config loading, and any CLI or UI.

## Layout

```
core/       conversation loop, events, tool interface, hooks, context compaction
provider/   OpenAI-compatible and Anthropic clients, retry, fake model for tests
session/    storage interface + in-memory store
tools/      tool registry + built-in bash / file tools
output/     optional output processing pipeline
gateway/    HTTP handler (REST + SSE)
adapter/    GORM storage
example/    runnable examples
```

The core packages do not import gin, gorm, zap or viper.

## Development

```
go test -race ./...
```

Known limits: run locks and rate limits are per process (no multi-instance
deployment yet); only SQLite has been tested with the GORM adapter.
