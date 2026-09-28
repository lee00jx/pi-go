// Command gin-integration is the integration story (DESIGN §11): the library
// gateway mounted on Gin via gin.WrapH, backed by real providers selected
// through env vars — an OpenAI-compatible one (or the scripted faux provider
// when PI_API_KEY is unset) plus Anthropic when ANTHROPIC_API_KEY is set.
// With two providers registered, a session can be switched mid-run with
// POST /api/agent/sessions/{id}/model (DESIGN §3.5) and the history travels
// between providers intact.
//
// Phase 5 (DESIGN §6.4, §7.3, §7.5, §7.6) wires the governance layer on top
// of that same seam: two-level context compaction (LLM summarizer bound to
// the default provider), per-run timeout/budget knobs, cost accounting, the
// in-memory multi-tenant rate limiter, and request_id idempotency (active
// whenever a client sends request_id — no extra wiring).
//
// The library itself never imports Gin (ADR-003): this example is the only
// place where a web framework touches pi-go, and it shows how thin that
// seam is (one WrapH). JWT/Casbin stay in the backend; this demo stamps
// ContextWithUserID from X-User-ID (default "demo") so sessions bind to a
// real user_id and create ignores any body field.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	gormstore "github.com/lee00jx/pi-go/adapter/gorm"
	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/gateway"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
	"github.com/lee00jx/pi-go/tools"
)

// nowTool is a natural first tool for a real LLM: it will call it
// whenever asked for the time, making the curl demo show a tool round.
type nowTool struct{}

func (nowTool) Name() string { return "now" }

func (nowTool) Description() string {
	return "Return the current UTC time in RFC3339. Call this when the user asks what time it is."
}

func (nowTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (nowTool) ExecutionMode() core.Mode { return core.ModeParallel }

func (nowTool) Execute(_ context.Context, _ core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	return core.ToolResult{Output: time.Now().UTC().Format(time.RFC3339)}, nil
}

// quoteTool demonstrates the business-API tool pattern: a tool that calls
// the integrator's own HTTP service. Point PI_QUOTE_URL at an endpoint
// that returns {"price": <number>} for GET {PI_QUOTE_URL}/{symbol};
// without it the tool answers with a stub quote so the demo still shows
// a full tool round against the model.
type quoteTool struct {
	url string
}

func (quoteTool) Name() string { return "quote" }

func (quoteTool) Description() string {
	return "Look up a stock quote by ticker symbol (e.g. AAPL). Returns price text."
}

func (quoteTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string","description":"ticker symbol, e.g. AAPL"}},"required":["symbol"]}`)
}

func (quoteTool) ExecutionMode() core.Mode { return core.ModeParallel }

func (q quoteTool) Execute(ctx context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	var args struct {
		Symbol string `json:"symbol"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	if args.Symbol == "" {
		return core.ToolResult{Output: "missing symbol", IsError: true}, nil
	}
	if q.url == "" {
		return core.ToolResult{Output: fmt.Sprintf("%s: 42.00 (stub — set PI_QUOTE_URL for live quotes)", args.Symbol)}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(q.url, "/")+"/"+args.Symbol, nil)
	if err != nil {
		return core.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return core.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return core.ToolResult{Output: fmt.Sprintf("quote API %d: %s", resp.StatusCode, string(body)), IsError: true}, nil
	}
	var qj struct {
		Price float64 `json:"price"`
	}
	if err := json.Unmarshal(body, &qj); err != nil {
		return core.ToolResult{Output: fmt.Sprintf("bad quote API response: %s", string(body)), IsError: true}, nil
	}
	return core.ToolResult{Output: fmt.Sprintf("%s: %.2f", args.Symbol, qj.Price)}, nil
}

// deployTool is the phase 3 demo's "dangerous" tool (DESIGN §5.3, §11):
// its BeforeToolCall hook returns Decision{Confirm:true}, so the run pauses
// on a tool_confirmation_request and waits for the user to answer
// POST /api/agent/sessions/{id}/confirmations before it may execute.
type deployTool struct{}

func (deployTool) Name() string { return "deploy" }

func (deployTool) Description() string {
	return "Deploy to an environment (staging or prod). This is a dangerous demo tool and requires user confirmation."
}

func (deployTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"env":{"type":"string","description":"target environment, e.g. staging or prod"}},"required":["env"]}`)
}

func (deployTool) ExecutionMode() core.Mode { return core.ModeParallel }

func (d deployTool) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	var args struct {
		Env string `json:"env"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	if args.Env == "" {
		args.Env = "staging"
	}
	return core.ToolResult{Output: fmt.Sprintf("deployed to %s (demo — nothing real happened)", args.Env)}, nil
}

// openGormStore opens a SQLite-backed GORMStore at dbPath (phase 4 persistence).
// It migrates the schema and returns the store plus a closer to release the DB
// file on shutdown. TranslateError maps constraint violations to gorm errors.
//
// A single run writes from two goroutines — the core runner appends messages
// and the gateway appends ui_events — so the SQLite DB sees concurrent writes.
// mattn/go-sqlite3 + a multi-connection pool returns SQLITE_BUSY under that
// load (the busy handler is unreliable across pooled connections), so we pin
// the pool to one connection; SQLite is single-writer anyway, so this only
// serializes what would have been serialized at the file level.
func openGormStore(dbPath string) (session.Store, func(), error) {
	db, err := gorm.Open(sqlite.Open(dbPath+"?_journal_mode=wal"), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, nil, err
	}
	if sqlDB, derr := db.DB(); derr == nil && sqlDB != nil {
		sqlDB.SetMaxOpenConns(1)
	}
	s := gormstore.New(db)
	closeFn := func() {
		if sqlDB, derr := db.DB(); derr == nil && sqlDB != nil {
			sqlDB.Close()
		}
	}
	if err := s.Migrate(context.Background()); err != nil {
		closeFn()
		return nil, nil, err
	}
	return s, closeFn, nil
}

// piDuration reads a time.Duration env var (e.g. "60s", "5m"); empty/unset or
// unparseable values yield def. Used for the phase 5 per-run timeout knobs so
// the example stays scriptable without recompiling.
func piDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// piInt reads an int env var; empty/unset or unparseable values yield def.
func piInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// costPerMillion prices a model for cost accounting (DESIGN §7.6). It returns
// USD per million tokens, (in, out). A small table covers the demo's models;
// anything unknown falls back to a flat default so the /usage endpoint always
// has a non-zero number to show. Swap this for a real price feed in production.
func costPerMillion(m core.Model) (in, out float64) {
	switch m.String() {
	case "openai/gpt-4o-mini", "faux/faux-mini":
		return 0.15, 0.60 // $/Mtok, gpt-4o-mini list price
	case "anthropic/claude-sonnet-4-20250514":
		return 3.00, 15.00
	default:
		return 3.00, 15.00 // conservative default for unknown models
	}
}

func main() {
	// Store selection (phase 4, DESIGN §6.5): sessions + events persist to a
	// SQLite file by default so a restart can resume an interrupted run.
	//   PI_DB_PATH unset      → default file ./pi-agent.db
	//   PI_DB_PATH=<path>     → that file
	//   PI_DB_PATH="" (empty) → in-memory (no persistence)
	var (
		store   session.Store
		closeDB = func() {}
	)
	if path, set := os.LookupEnv("PI_DB_PATH"); !set || path != "" {
		p := path
		if p == "" {
			p = "./pi-agent.db"
		}
		s, close, err := openGormStore(p)
		if err != nil {
			log.Fatalf("open store %s: %v", p, err)
		}
		store, closeDB = s, close
		log.Printf("store: sqlite @ %s (sessions + events persist across restarts)", p)
	} else {
		store = session.NewMemoryStore()
		log.Println("store: in-memory (PI_DB_PATH is empty)")
	}

	// The DEFAULT provider: OpenAI-compatible (env), or faux (no key).
	var streamFn core.StreamFn
	var model core.Model
	if key := os.Getenv("PI_API_KEY"); key != "" {
		base := os.Getenv("PI_BASE_URL")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		providerName := os.Getenv("PI_PROVIDER")
		if providerName == "" {
			providerName = "openai"
		}
		id := os.Getenv("PI_MODEL")
		if id == "" {
			id = "gpt-4o-mini"
		}
		oa, err := provider.NewOpenAI(base, key)
		if err != nil {
			log.Fatal(err)
		}
		oa.Provider = providerName
		// nil config = DESIGN §3.4 defaults (3 retries, 500ms base, 8s cap).
		streamFn = provider.WithRetry(oa.Stream, nil)
		model = core.Model{Provider: providerName, ID: id, ContextWindow: 128000}
		log.Printf("default provider: %s/%s @ %s (OpenAI-compatible)", providerName, id, base)
	} else {
		log.Println("default provider: PI_API_KEY not set — scripted faux provider (no tokens spent)")
		faux := provider.NewFaux(
			core.Model{Provider: "faux", ID: "faux-mini", ContextWindow: 128000},
			provider.FauxTurn{
				Text:      "Let me check the clock and a quote for you.",
				ToolCalls: []provider.FauxToolCall{{Name: "now"}, {Name: "quote", Args: json.RawMessage(`{"symbol":"AAPL"}`)}},
			},
			// Turn 2 calls the dangerous `deploy` tool — the BeforeToolCall
			// hook routes it to confirmation, so the run pauses here until
			// the user answers (DESIGN §5.3).
			provider.FauxTurn{
				Text:      "Now let me deploy to staging — this one needs your OK first.",
				ToolCalls: []provider.FauxToolCall{{Name: "deploy", Args: json.RawMessage(`{"env":"staging"}`)}},
			},
			provider.FauxTurn{
				Text: "All done. Set PI_API_KEY and restart to talk to a real LLM — everything else stays the same.",
			},
		)
		streamFn = faux.Stream
		model = faux.Model
	}

	// All registered providers. The runner resolves each session's stored
	// (provider, model) against this map at run start and every turn
	// boundary, which is what makes POST /model work mid-run (DESIGN §3.5).
	providers := map[string]core.ProviderBinding{
		model.Provider: {StreamFn: streamFn, Model: model},
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		an, err := provider.NewAnthropic(key)
		if err != nil {
			log.Fatal(err)
		}
		anID := os.Getenv("PI_ANTHROPIC_MODEL")
		if anID == "" {
			anID = "claude-sonnet-4-20250514"
		}
		fn := provider.WithRetry(an.Stream, nil)
		providers["anthropic"] = core.ProviderBinding{StreamFn: fn, Model: core.Model{Provider: "anthropic", ID: anID, ContextWindow: 200000}}
		log.Printf("registered provider: anthropic/%s", anID)
	}

	// core.RecallEventTool (a SessionTool) is the L1-archive escape hatch:
	// when compaction evicts an old tool result into "[结果已归档,seq=N,...]",
	// the model calls recall_event{seq:N} to read the original back. It reads
	// from the same store the runner persists to.
	// Built-in bash/fs tools (1:1 with pi, no sandbox). Paths and command
	// working dirs resolve against PI_WORK_DIR (default: the process cwd).
	workDir := os.Getenv("PI_WORK_DIR")
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}
	reg := tools.NewRegistry(
		nowTool{}, quoteTool{url: os.Getenv("PI_QUOTE_URL")}, deployTool{},
		core.RecallEventTool{Store: store},
		tools.NewBash(workDir),
		tools.NewRead(workDir),
		tools.NewWrite(workDir),
		tools.NewEdit(workDir),
	)
	runner := &core.Runner{
		Store:     store,
		StreamFn:  streamFn, // fallback for sessions whose provider has no binding
		Tools:     reg.List(),
		Model:     model,
		Providers: providers,
		SystemPrompt: "You are a helpful assistant with a `now` tool for the current UTC " +
			"time, a `quote` tool for ticker quotes, a `deploy` tool for releases, a " +
			"`bash` tool to run shell commands, and `read`/`write`/`edit` tools for file " +
			"operations (paths resolve against the working directory). " +
			"If you see a placeholder like [结果已归档,seq=N,可用 recall_event 取回], the " +
			"original tool result was archived to save context — call `recall_event` with " +
			"that seq to fetch it back.",
		MaxTurns: 8,
		Logger:   slog.Default(),

		// Phase 5 (DESIGN §6.4): two-level context compaction. L1 (cheap,
		// no LLM) evicts old tool results once EvictOlderThan user turns have
		// passed; L2 summarizes the older tail into a compaction entry when
		// the derived context exceeds the model window minus ReserveTokens.
		// The summarizer is bound to the default provider via SummarizeWithStream
		// — an independent no-tool call, so a misbehaving model (tool call /
		// empty) abandons the compaction fail-safe and retries next turn.
		Compaction: func() core.CompactionConfig {
			cfg := core.DefaultCompaction()
			cfg.Summarize = core.SummarizeWithStream(streamFn)
			return cfg
		}(),
		// Phase 5 (DESIGN §7.4/§7.6): resource governance. Per-LLM-turn and
		// per-tool deadlines, a per-session cumulative token budget, and the
		// price table that turns usage into dollars on /usage. All env-tunable.
		TurnTimeout:    piDuration("PI_TURN_TIMEOUT", 120*time.Second),
		ToolTimeout:    piDuration("PI_TOOL_TIMEOUT", 60*time.Second),
		TokenBudget:    piInt("PI_TOKEN_BUDGET", 0), // 0 = unlimited (demo default)
		CostPerMillion: costPerMillion,
		// Phase 3 (DESIGN §5.3): route dangerous calls to confirmation. The
		// gateway installs the Web ConfirmTool waiter, so a Confirm decision
		// pauses the run and waits for POST /confirmations (default 120s
		// timeout = deny). Reason doubles as the human-facing risk text.
		Hooks: core.Hooks{
			BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
				if call.Name == "deploy" {
					return core.Decision{Confirm: true, Reason: "deploy is irreversible"}, nil
				}
				return core.Decision{}, nil
			},
		},
	}
	coder := *runner
	coder.SystemPrompt = "You are a terse coding assistant with the same tools as the default assistant. Prefer short answers and code."
	// Phase 5 (DESIGN §7.5): in-memory multi-tenant rate limiter — per-user
	// concurrent-run cap plus a prompts-per-minute sliding window. Both are
	// env-tunable; a value of 0 disables that dimension. Defaults here are
	// lenient enough that the curl demo never trips a 429, but real
	// multi-tenant deployments tighten them.
	maxConcurrent := piInt("PI_MAX_CONCURRENT", 4)
	maxPromptsPerMin := piInt("PI_MAX_PROMPTS_PER_MIN", 30)
	gw := gateway.New(store, runner,
		gateway.WithAgentRunner("assistant", runner),
		gateway.WithAgentRunner("coder", &coder),
		gateway.WithRateLimit(maxConcurrent, maxPromptsPerMin),
	)

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	// Non-agent routes live on the framework; the agent API is the library
	// gateway, mounted wholesale — no per-route glue.
	r.GET("/health", func(c *gin.Context) {
		names := make([]string, 0, len(providers))
		for p := range providers {
			names = append(names, p)
		}
		c.JSON(200, gin.H{"ok": true, "default": model.Provider + "/" + model.ID, "providers": names})
	})
	// JWT/Casbin stay in the Go backend. Stamp the verified user into
	// context so pi-go can bind sessions.user_id and check ownership;
	// create ignores any body user_id. This demo uses X-User-ID (default
	// "demo"); a real app reads the claim from gin-jwt after Casbin.
	protected := r.Group("/")
	protected.Use(func(c *gin.Context) {
		user := c.GetHeader("X-User-ID")
		if user == "" {
			user = "demo"
		}
		c.Request = c.Request.WithContext(gateway.ContextWithUserID(c.Request.Context(), user))
		c.Next()
	})
	// *catch is a gin catch-all; the wrapped handler sees the full request
	// path, so the library's ServeMux patterns match unchanged.
	protected.Any("/api/agent/*catch", gin.WrapH(gw))

	banner := `
  pi-go + Gin listening on :8080

  1. create a session (agent is required when WithAgentRunner is set):
     curl -s -X POST localhost:8080/api/agent/sessions -d '{"agent":"assistant"}'
     curl -s localhost:8080/api/agent/sessions?agent=assistant
  2. watch the event stream (start BEFORE step 3):
     curl -N localhost:8080/api/agent/sessions/<id>/events
  3. send a prompt (triggers the ` + "`now`" + ` and ` + "`quote`" + ` tools):
     curl -s -X POST localhost:8080/api/agent/sessions/<id>/prompts -d '{"text":"What time is it, and what is AAPL at? Check with the tools."}'
  4. the ` + "`deploy`" + ` tool needs confirmation (DESIGN §5.3). While the
     stream is open you'll see a tool_confirmation_request; answer it:
     curl -s -X POST localhost:8080/api/agent/sessions/<id>/confirmations -d '{"id":"<confirmationId>","decision":"allow"}'
  5. first-screen history (non-streaming) and per-prompt turns:
     curl -s localhost:8080/api/agent/sessions/<id>/messages
     curl -s localhost:8080/api/agent/sessions/<id>/turns

  phase 4 — persistence & recovery (DESIGN §6.5):
    • state lives in SQLite (PI_DB_PATH, default ./pi-agent.db; set PI_DB_PATH=""
      for in-memory). Sessions + every event survive a restart.
    • the run also keeps going if the SSE client disconnects; reconnect with
      ?after=<seq> (or Last-Event-ID) to catch up exactly.
    • graceful stop:  kill -TERM <pid>  cancels in-flight runs, marks the
      session ` + "`interrupted`" + `, then exits.
    • after RESTARTING the server, resume an interrupted session from its last
      complete message:
     curl -s -X POST localhost:8080/api/agent/sessions/<id>/resume -d '{}'

  phase 5 — compaction & governance (DESIGN §6.4, §7.3, §7.5, §7.6):
    • context usage bar (currentTokens / contextWindow / usedPercent /
      how many L2 compactions so far):
     curl -s localhost:8080/api/agent/sessions/<id>/context-stats
    • cumulative per-session usage + cost (folded in at run end):
     curl -s localhost:8080/api/agent/sessions/<id>/usage
    • two-level compaction runs automatically between turns: L1 archives old
      tool results, L2 summarizes the older tail into a compaction entry when
      the context nears the window (summarizer = default provider).
    • idempotent retries: resubmitting a prompt with the same
      "request_id" returns 202 "duplicate" and does NOT re-run the loop.
    • rate limiting (PI_MAX_CONCURRENT / PI_MAX_PROMPTS_PER_MIN, 0 = off):
      over the per-user concurrent-run or prompts/min cap → HTTP 429.
`
	if _, ok := providers["anthropic"]; ok {
		banner += `  6. switch this session's model mid-run (DESIGN §3.5; takes effect
     at the next turn boundary, history carries over):
     curl -s -X POST localhost:8080/api/agent/sessions/<id>/model -d '{"provider":"anthropic","model":"claude-sonnet-4-20250514"}'
`
	}
	log.Print(banner)

	// Serve until SIGTERM/SIGINT, then shut down gracefully (DESIGN §6.5):
	// cancel in-flight runs (marking them interrupted so a restarted process
	// can resume them), stop accepting new connections, then release the store.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: ":8080", Handler: r}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatal(err)
	case <-sigCtx.Done():
	}
	stop() // stop watching for further signals
	log.Println("signal received — shutting down gracefully (cancelling runs, marking them interrupted)")
	gw.Shutdown()

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	closeDB()
	log.Println("bye")
}
