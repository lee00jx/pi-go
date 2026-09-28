// Package gateway exposes the agent over HTTP as plain stdlib
// http.Handlers (ADR-003): mount it with net/http, Gin, Echo, ... — the
// library never imports a web framework.
//
// It is the only library package that touches HTTP, and it is where events
// get persisted: every AgentEvent is appended as a ui_event entry (sharing
// the per-session seq space with messages) and then broadcast to live SSE
// subscribers. That ordering is what makes reconnect replay exact
// (DESIGN §6.5).
//
// Endpoints: POST /sessions, GET /sessions, GET /sessions/{id}/turns,
// POST /sessions/{id}/prompts (steers when running), GET /sessions/{id}/events
// (SSE), GET /sessions/{id}/messages (first-screen, non-streaming),
// POST /sessions/{id}/stop, POST /sessions/{id}/model (mid-run model switch,
// DESIGN §3.5), POST /sessions/{id}/confirmations (dangerous-op allow/deny,
// DESIGN §5.3), GET /sessions/{id}/usage (cost accounting, DESIGN §7.6) and
// GET /sessions/{id}/context-stats (occupancy bar, DESIGN §8).
// request_id idempotency (DESIGN §7.3) and multi-tenant rate limiting
// (DESIGN §7.5) land in phase 5.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/session"
)

// Gateway implements http.Handler.
type Gateway struct {
	store            session.Store
	runner           *core.Runner
	agents           map[string]*core.Runner // WithAgentRunner; nil/empty = single default
	hub              *hub
	handler          http.Handler
	base             string
	eventMiddlewares []EventMiddleware
	auth             []func(http.Handler) http.Handler
	authorizer       Authorizer
	anonUser         string // WithAnonymousUser fallback (dev/tests only)

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	steering map[string]*steeringQueue
	confirms map[string]*pendingConfirm
	confirmT time.Duration

	// shuttingDown is set by Shutdown(); finishRun uses it to mark a
	// cancel-ended run interrupted (resumable) rather than idle, and
	// startLoop refuses new runs so Shutdown's wait can complete.
	shuttingDown atomic.Bool
	// runWG tracks in-flight runs so Shutdown can wait for them to wind
	// down (and persist their interrupted status) before returning.
	runWG sync.WaitGroup

	// Multi-tenant governance (DESIGN §7.5/§7.3).
	limiter *limiter // nil = no rate limiting (lenient default)
	dedupMu sync.Mutex
	dedup   map[string]map[string]struct{} // sessionID → seen request keys
}

// errShuttingDown is returned by startLoop when a new run is requested
// after Shutdown() has begun.
var errShuttingDown = errors.New("gateway: shutting down")

// pendingConfirm is one in-flight confirmation request: the loop is blocked
// in the ConfirmTool hook until a client answers via POST /confirmations
// (or the timeout, or the run is stopped). Keyed by the tool call's id,
// which is also the confirmationId the frontend posts back.
type pendingConfirm struct {
	sessionID string
	decision  chan core.Confirmation
}

// steeringQueue is one session's queue of user messages typed while the
// agent is running (DESIGN §2.5). They are injected at the next turn
// boundary; the in-memory queue survives browser refreshes because the run
// lives on the server side.
type steeringQueue struct {
	mu   sync.Mutex
	msgs []*core.Message
}

func (q *steeringQueue) push(m *core.Message) {
	q.mu.Lock()
	q.msgs = append(q.msgs, m)
	q.mu.Unlock()
}

// drain returns and clears the queue (the loop calls it once per turn).
func (q *steeringQueue) drain() []*core.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	msgs := q.msgs
	q.msgs = nil
	return msgs
}

// Option customizes the gateway.
type Option func(*Gateway)

// EventMiddleware optionally transforms the Runner event stream before the
// gateway persists and broadcasts it. Implementations must close their
// returned channel after source closes.
type EventMiddleware func(context.Context, <-chan core.AgentEvent) <-chan core.AgentEvent

// WithEventMiddleware appends event-stream middleware in execution order.
// It is the gateway integration point for output.Transform; nil middleware
// is ignored.
func WithEventMiddleware(middlewares ...EventMiddleware) Option {
	return func(g *Gateway) {
		for _, middleware := range middlewares {
			if middleware != nil {
				g.eventMiddlewares = append(g.eventMiddlewares, middleware)
			}
		}
	}
}

// Authorizer is an escalation hook on top of the default owner check: it
// runs only after the owner check failed, so it can widen access (e.g.
// let admins through) but never weaken it. A nil error allows the
// request; any error maps to HTTP 404 so unauthorized callers cannot
// probe session ids.
type Authorizer func(ctx context.Context, meta session.SessionMeta) error

// WithAuthorizer adds the escalation hook described by Authorizer. The
// default owner check (session.UserID == authenticated user) always
// runs first.
func WithAuthorizer(fn Authorizer) Option {
	return func(g *Gateway) {
		if fn != nil {
			g.authorizer = fn
		}
	}
}

// WithAnonymousUser stamps user into requests that carry no
// authenticated user — for local development, examples, and tests.
// Production deployments must omit it: without an identity the gateway
// answers 401 (fail-closed) instead of skipping the ownership check.
func WithAnonymousUser(user string) Option {
	return func(g *Gateway) {
		if user != "" {
			g.anonUser = user
		}
	}
}

// WithAuthMiddleware wraps the whole router with the integrator's HTTP
// wrapper (DESIGN §5.1). JWT and Casbin stay in the Go backend — this is
// only an optional mount point. Prefer stamping ContextWithUserID on a
// protected route group and mounting the gateway there. Wrappers are
// applied after routes are registered, so they stay valid when combined
// with WithBasePath. Multiple wrappers nest in option order: the first is
// innermost.
func WithAuthMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(g *Gateway) {
		if mw != nil {
			g.auth = append(g.auth, mw)
		}
	}
}

// WithBasePath overrides the default "/api/agent" route prefix.
func WithBasePath(p string) Option {
	return func(g *Gateway) { g.base = strings.TrimRight(p, "/") }
}

// WithConfirmTimeout overrides the confirmation wait (default 120s).
// The timeout counts as a denial (DESIGN §5.3).
func WithConfirmTimeout(d time.Duration) Option {
	return func(g *Gateway) {
		if d > 0 {
			g.confirmT = d
		}
	}
}

// WithRateLimit enables in-memory multi-tenant rate limiting (DESIGN §7.5):
// at most maxConcurrent runs in flight per user and at most
// maxPromptsPerMin prompt submissions per user per rolling minute. Exceeding
// either returns HTTP 429. A zero value disables that dimension; without this
// option no limiting is applied (the lenient default).
func WithRateLimit(maxConcurrent, maxPromptsPerMin int) Option {
	return func(g *Gateway) {
		g.limiter = newLimiter(maxConcurrent, maxPromptsPerMin)
	}
}

// WithAgentRunner registers a Runner for sessions whose SessionMeta.Agent
// matches agent. When at least one mapping is registered, create must send
// a registered agent (400 otherwise) and prompt/resume/model/context-stats
// on an unknown type return 503. Without this option the default runner is
// used and agent may be empty.
func WithAgentRunner(agent string, r *core.Runner) Option {
	return func(g *Gateway) {
		if agent == "" || r == nil {
			return
		}
		if g.agents == nil {
			g.agents = make(map[string]*core.Runner)
		}
		g.agents[agent] = r
	}
}

// New builds a Gateway. The returned value is itself an http.Handler.
func New(store session.Store, runner *core.Runner, opts ...Option) *Gateway {
	g := &Gateway{
		store:    store,
		runner:   runner,
		agents:   make(map[string]*core.Runner),
		hub:      newHub(),
		base:     "/api/agent",
		cancels:  make(map[string]context.CancelFunc),
		steering: make(map[string]*steeringQueue),
		confirms: make(map[string]*pendingConfirm),
		confirmT: 120 * time.Second, // DESIGN §5.3 default; timeout = deny
		dedup:    make(map[string]map[string]struct{}),
	}
	// Construction options (base path, agent map, auth wrappers, timeouts, …)
	// must run before the mux is built and before runners are wired:
	// WithBasePath is baked into patterns, WithAgentRunner fills the map,
	// and WithAuthMiddleware wraps the finished handler.
	for _, o := range opts {
		if o != nil {
			o(g)
		}
	}
	wired := make(map[*core.Runner]struct{})
	g.wireRunner(runner, wired)
	for _, r := range g.agents {
		g.wireRunner(r, wired)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+g.base+"/sessions", g.handleCreate)
	mux.HandleFunc("GET "+g.base+"/sessions", g.handleList)
	mux.HandleFunc("POST "+g.base+"/sessions/{id}/prompts", g.handlePrompt)
	mux.HandleFunc("GET "+g.base+"/sessions/{id}/events", g.handleEvents)
	mux.HandleFunc("GET "+g.base+"/sessions/{id}/messages", g.handleMessages)
	mux.HandleFunc("GET "+g.base+"/sessions/{id}/turns", g.handleTurns)
	mux.HandleFunc("POST "+g.base+"/sessions/{id}/stop", g.handleStop)
	mux.HandleFunc("POST "+g.base+"/sessions/{id}/model", g.handleModel)
	mux.HandleFunc("POST "+g.base+"/sessions/{id}/confirmations", g.handleConfirm)
	mux.HandleFunc("POST "+g.base+"/sessions/{id}/resume", g.handleResume)
	mux.HandleFunc("GET "+g.base+"/sessions/{id}/usage", g.handleUsage)
	mux.HandleFunc("GET "+g.base+"/sessions/{id}/context-stats", g.handleContextStats)
	g.handler = mux
	for _, mw := range g.auth {
		g.handler = mw(g.handler)
	}
	return g
}

// wireRunner attaches this gateway's steering queue and (when unset) the
// Web confirmation waiter. Pointer-equal runners are wired once so a
// default that is also registered via WithAgentRunner is not double-wrapped.
func (g *Gateway) wireRunner(runner *core.Runner, wired map[*core.Runner]struct{}) {
	if runner == nil {
		return
	}
	if _, ok := wired[runner]; ok {
		return
	}
	wired[runner] = struct{}{}
	origSteering := runner.Hooks.GetSteeringMessages
	runner.Hooks.GetSteeringMessages = func(ctx context.Context, sessionID string) []*core.Message {
		var msgs []*core.Message
		if origSteering != nil {
			msgs = origSteering(ctx, sessionID)
		}
		if q := g.steeringQueue(sessionID); q != nil {
			msgs = append(msgs, q.drain()...)
		}
		return msgs
	}
	if runner.Hooks.ConfirmTool == nil {
		runner.Hooks.ConfirmTool = g.waitConfirm
	}
}

func (g *Gateway) steeringQueue(id string) *steeringQueue {
	g.mu.Lock()
	defer g.mu.Unlock()
	q, ok := g.steering[id]
	if !ok {
		q = &steeringQueue{}
		g.steering[id] = q
	}
	return q
}

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handler.ServeHTTP(w, r)
}

// Shutdown gracefully stops the gateway (DESIGN §6.5). It marks the gateway
// as shutting down (so no new runs start and cancel-ended runs are recorded
// interrupted), cancels every in-flight run, and waits for them to wind down
// so their interrupted status and any partial output are persisted before the
// process exits. The integrator calls it from its SIGTERM/SIGINT handler.
func (g *Gateway) Shutdown() {
	if !g.shuttingDown.CompareAndSwap(false, true) {
		// Already shutting down; just wait for the in-flight runs.
		g.runWG.Wait()
		return
	}
	g.mu.Lock()
	for _, cancel := range g.cancels {
		cancel()
	}
	g.cancels = make(map[string]context.CancelFunc)
	g.mu.Unlock()
	g.runWG.Wait()
}

// --- handlers -----------------------------------------------------------

func (g *Gateway) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Title    string `json:"title"`
		Agent    string `json:"agent"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	// Identity comes only from the authenticated context. A client-supplied
	// user_id in the body is ignored so callers cannot mint sessions as
	// someone else (DESIGN §5.1). Fail-closed: without a user (and without
	// the WithAnonymousUser dev fallback) create answers 401.
	userID := g.currentUser(r.Context())
	if userID == "" {
		writeErr(w, http.StatusUnauthorized, "no authenticated user")
		return
	}
	runner, err := g.runnerForCreate(body.Agent)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	provider := body.Provider
	if provider == "" {
		provider = runner.Model.Provider
	}
	model := body.Model
	if model == "" {
		model = runner.Model.ID
	}
	// The stored (provider, model) is what routes this session's LLM
	// calls: at run start and at every turn boundary the runner resolves
	// it against Runner.Providers (DESIGN §3.5). Agent is immutable after
	// create.
	meta := session.SessionMeta{
		ID:        newSessionID(),
		UserID:    userID,
		Provider:  provider,
		Model:     model,
		Status:    session.StatusIdle,
		Title:     body.Title,
		Agent:     body.Agent,
		CreatedAt: time.Now(),
	}
	if err := g.store.CreateSession(r.Context(), meta); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": meta.ID})
}

const (
	listDefaultLimit = 50
	listMaxLimit     = 200
)

func (g *Gateway) handleList(w http.ResponseWriter, r *http.Request) {
	userID := g.currentUser(r.Context())
	if userID == "" {
		writeErr(w, http.StatusUnauthorized, "no authenticated user")
		return
	}
	limit := listDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 0 {
			limit = n
		}
		if limit > listMaxLimit {
			limit = listMaxLimit
		}
	}
	list, err := g.store.ListSessions(r.Context(), session.SessionQuery{
		UserID: userID,
		Agent:  r.URL.Query().Get("agent"),
		Limit:  limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		ID        string                `json:"id"`
		Agent     string                `json:"agent,omitempty"`
		Title     string                `json:"title,omitempty"`
		Status    session.SessionStatus `json:"status"`
		Provider  string                `json:"provider"`
		Model     string                `json:"model"`
		CreatedAt time.Time             `json:"createdAt"`
		UpdatedAt time.Time             `json:"updatedAt"`
	}
	out := make([]item, 0, len(list))
	for _, m := range list {
		out = append(out, item{
			ID: m.ID, Agent: m.Agent, Title: m.Title, Status: m.Status,
			Provider: m.Provider, Model: m.Model,
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) handlePrompt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	meta, err := g.sessionFor(ctx, id)
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	if _, err := g.runnerFor(meta); err != nil {
		writeAgentErr(w, err)
		return
	}
	var body struct {
		Text      string `json:"text"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, `field "text" is required`)
		return
	}
	// request_id idempotency (DESIGN §7.3): a repeated request must not
	// re-trigger a run. The key is claimed before the run starts so two
	// racing duplicates cannot both start a run; every failure path
	// after the claim rolls it back (#7 fix), so a client that received
	// 429/500/503 may safely retry with the same request_id.
	var key string
	accepted := false
	if body.RequestID != "" {
		key = "p:" + body.RequestID
		if g.requestDup(id, key) {
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "duplicate"})
			return
		}
		defer func() {
			if !accepted {
				g.requestRollback(id, key)
			}
		}()
	}
	// Multi-tenant rate limit (DESIGN §7.5): every prompt is a rate event.
	// sessionFor already required an identity, so currentUser is the caller
	// (JWT user or WithAnonymousUser fallback) — not a fourth lookup path.
	if !g.limiter.allowPrompt(g.currentUser(ctx)) {
		writeErr(w, http.StatusTooManyRequests, "prompt rate limit exceeded")
		return
	}
	msg := core.NewUserMessage(body.Text)
	msg.PromptID = newPromptID()

	runID, err := g.startRun(id, []*core.Message{msg})
	if err == session.ErrSessionBusy {
		// Steering (DESIGN §2.5): the session is running, so queue the
		// message; the loop injects it at the next turn boundary.
		// Queued is a new prompt (P) with no run_id yet — R is bound
		// when this run absorbs it or a follow-up startLoop begins.
		g.steeringQueue(id).push(msg)
		accepted = true
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":    "queued",
			"prompt_id": msg.PromptID,
		})
		return
	}
	if err == errShuttingDown {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err == errUnknownAgent {
		writeAgentErr(w, err)
		return
	}
	if err == errRateLimited {
		writeErr(w, http.StatusTooManyRequests, "concurrent session limit exceeded")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	accepted = true
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":    "started",
		"prompt_id": msg.PromptID,
		"run_id":    runID,
	})
}

// startLoop is the shared run bootstrap: acquire the per-session lock, mark
// the session running, kick off the loop via begin (which returns the event
// channel), stamp run/prompt/turn after EventMiddleware, then spawn the
// goroutine that persists + broadcasts every event until the stream closes.
// It returns the new run_id, or session.ErrSessionBusy when another run
// holds the lock (the caller decides whether to steer), errShuttingDown
// after Shutdown() has begun, or another error when the run could not start.
// The run is decoupled from any request: a client disconnect must not stop
// the agent (DESIGN §6.5 "断线继续运行").
func (g *Gateway) startLoop(id, promptID string, begin func(ctx context.Context) (<-chan core.AgentEvent, error)) (string, error) {
	if g.shuttingDown.Load() {
		return "", errShuttingDown
	}
	release, err := g.store.AcquireLock(context.Background(), id)
	if err != nil {
		return "", err
	}
	// Per-user concurrency cap (DESIGN §7.5): reject before the run takes
	// resources if the user is already at their in-flight run limit.
	userID := g.sessionUserID(id)
	if !g.limiter.allowConcurrent(userID) {
		release()
		return "", errRateLimited
	}
	runCtx, cancel := context.WithCancel(context.Background())
	g.mu.Lock()
	g.cancels[id] = cancel
	g.mu.Unlock()
	g.runWG.Add(1)
	g.setStatus(context.Background(), id, session.StatusRunning)

	events, err := begin(runCtx)
	if err != nil {
		cancel()
		g.runWG.Done()
		g.limiter.releaseConcurrent(userID)
		release()
		g.releaseCancel(id)
		g.setStatus(context.Background(), id, session.StatusIdle)
		return "", err
	}
	for _, middleware := range g.eventMiddlewares {
		if transformed := middleware(runCtx, events); transformed != nil {
			events = transformed
		}
	}
	runID := newRunID()
	events = stampRunIDs(runID, promptID, events)

	log := slog.Default().With("session", id, "component", "gateway")
	go func() {
		defer g.runWG.Done()
		for ev := range events {
			data, _ := json.Marshal(ev)
			seq, err := g.store.AppendEntry(runCtx, id, session.EntryUIEvent, data, nil)
			if err != nil {
				// Keep draining so the run can finish even while persistence
				// is failing. A cancelled run context (graceful shutdown)
				// makes the remaining appends fail by design — that is
				// expected, so don't log a warning per event for it. We also
				// skip the broadcast: an event that failed to persist can't be
				// replayed on reconnect, so it shouldn't be shown live either.
				if runCtx.Err() == nil {
					log.Warn("persist event failed", "err", err)
				}
				continue
			}
			g.hub.broadcast(id, sseFrame{ID: strconv.FormatInt(seq, 10), Data: string(data)})
		}
		g.finishRun(id, userID, release, runCtx)
	}()
	return runID, nil
}

// startRun begins a fresh run for the session with the given user prompts.
func (g *Gateway) startRun(id string, prompts []*core.Message) (string, error) {
	promptID := ""
	if len(prompts) > 0 {
		promptID = prompts[0].PromptID
	}
	return g.startLoop(id, promptID, func(ctx context.Context) (<-chan core.AgentEvent, error) {
		runner, err := g.runnerForID(id)
		if err != nil {
			return nil, err
		}
		return runner.Run(ctx, id, prompts)
	})
}

// startContinue resumes an interrupted session with no new prompt
// (DESIGN §6.5 agentLoopContinue): the loop rebuilds context from the store
// and asks the model to continue from the last complete message.
func (g *Gateway) startContinue(id string) (string, error) {
	promptID := lastUserPromptID(context.Background(), g.store, id)
	return g.startLoop(id, promptID, func(ctx context.Context) (<-chan core.AgentEvent, error) {
		runner, err := g.runnerForID(id)
		if err != nil {
			return nil, err
		}
		return runner.RunContinue(ctx, id)
	})
}

// lastUserPromptID walks entries newest-first and returns the PromptID on
// the most recent user message. Resume does not mint a new P.
func lastUserPromptID(ctx context.Context, store session.Store, id string) string {
	entries, err := store.ListEntries(ctx, id, 0, 0)
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type != session.EntryUser {
			continue
		}
		var m core.Message
		if json.Unmarshal(entries[i].Payload, &m) != nil {
			continue
		}
		if m.PromptID != "" {
			return m.PromptID
		}
	}
	return ""
}

// stampRunIDs copies run/prompt/turn onto every event after EventMiddleware
// so an integrator Transform does not have to forward the new fields.
func stampRunIDs(runID, promptID string, src <-chan core.AgentEvent) <-chan core.AgentEvent {
	out := make(chan core.AgentEvent, 64)
	go func() {
		defer close(out)
		turn := 0
		for ev := range src {
			ev.RunID = runID
			if ev.Type == core.TurnStart && ev.Turn > 0 {
				turn = ev.Turn
			}
			if ev.Turn == 0 && turn > 0 {
				ev.Turn = turn
			}
			if ev.Message != nil && ev.Message.PromptID != "" {
				promptID = ev.Message.PromptID
			}
			if ev.PromptID == "" {
				ev.PromptID = promptID
			}
			out <- ev
		}
	}()
	return out
}

// finishRun runs when the event stream closes. If the run was cancelled
// during a graceful shutdown it marks the session interrupted (so a
// restarted process can resume it from the last complete message) and does
// not start any follow-up; otherwise a queued steering message starts a
// follow-up run or the session goes idle. Ordering matters in the follow-up
// case — release the lock BEFORE startRun so the new run can take it; if a
// racing prompt wins the lock in between, the messages are re-queued so that
// run picks them up.
func (g *Gateway) finishRun(id string, userID string, release func(), runCtx context.Context) {
	g.releaseCancel(id)
	g.limiter.releaseConcurrent(userID)
	if g.shuttingDown.Load() && runCtx.Err() == context.Canceled {
		release()
		g.setStatus(context.Background(), id, session.StatusInterrupted)
		return
	}
	pending := g.steeringQueue(id).drain()
	if len(pending) > 0 {
		release()
		if _, err := g.startRun(id, pending); err == session.ErrSessionBusy {
			for _, m := range pending {
				g.steeringQueue(id).push(m)
			}
		}
		return
	}
	release()
	g.setStatus(context.Background(), id, session.StatusIdle)
}

// handleMessages is the first-screen endpoint (DESIGN §6.5): the full
// message history without the event stream.
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	if _, err := g.sessionFor(ctx, id); err != nil {
		writeSessionErr(w, err)
		return
	}
	entries, err := g.store.ListEntries(ctx, id, 0, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type msgView struct {
		Seq       int64     `json:"seq"`
		CreatedAt time.Time `json:"createdAt,omitempty"`
		*core.Message
	}
	msgs := make([]msgView, 0, len(entries))
	for _, e := range entries {
		switch e.Type {
		case session.EntryUser, session.EntryAssistant, session.EntryToolResult:
		default:
			continue
		}
		var m core.Message
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			continue
		}
		msgs = append(msgs, msgView{Seq: e.Seq, CreatedAt: e.CreatedAt, Message: &m})
	}
	writeJSON(w, http.StatusOK, msgs)
}

// handleEvents is the SSE core endpoint (DESIGN §8): replay missed events
// (Last-Event-ID or ?after=seq), then stream live, with a 15s heartbeat.
func (g *Gateway) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := g.sessionFor(r.Context(), id); err != nil {
		writeSessionErr(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var after int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}

	sub, cancel := g.hub.subscribe(id)
	defer cancel()

	// Replay: re-send every ui_event persisted after `after`, in order.
	if entries, err := g.store.ListEntries(r.Context(), id, after, 0); err == nil {
		for _, e := range entries {
			if e.Type != session.EntryUIEvent {
				continue
			}
			writeSSEFrame(w, sseFrame{ID: strconv.FormatInt(e.Seq, 10), Data: string(e.Payload)})
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-sub:
			writeSSEFrame(w, f)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// waitConfirm is the gateway's ConfirmTool implementation (DESIGN §5.3):
// it registers the pending request (so POST /confirmations can find it)
// and blocks until the decision, the timeout, or the run's ctx ends.
// Timeout and stop both count as denials — a confirm never becomes a
// silent allow. The tool_confirmation_request/response events around the
// call are emitted by the loop and persisted like any other event, which
// is what restores the pending state after a refresh.
func (g *Gateway) waitConfirm(ctx context.Context, sessionID string, call core.ToolCall) (core.Confirmation, error) {
	p := &pendingConfirm{sessionID: sessionID, decision: make(chan core.Confirmation, 1)}
	g.mu.Lock()
	g.confirms[call.ID] = p // a re-issued call id replaces the stale pending
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if cur, ok := g.confirms[call.ID]; ok && cur == p {
			delete(g.confirms, call.ID)
		}
		g.mu.Unlock()
	}()

	timer := time.NewTimer(g.confirmT)
	defer timer.Stop()
	select {
	case c := <-p.decision:
		return c, nil
	case <-timer.C:
		return core.Confirmation{Allowed: false, Reason: "confirmation timed out"}, nil
	case <-ctx.Done():
		return core.Confirmation{Allowed: false, Reason: "the run was stopped before confirmation"}, nil
	}
}

// handleConfirm answers a pending confirmation (DESIGN §5.3). The id is
// the confirmationId from the tool_confirmation_request event (= the tool
// call's id); decision is "allow" or "deny".
func (g *Gateway) handleConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	if _, err := g.sessionFor(ctx, id); err != nil {
		writeSessionErr(w, err)
		return
	}
	var body struct {
		ID        string `json:"id"`
		Decision  string `json:"decision"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeErr(w, http.StatusBadRequest, `field "id" is required`)
		return
	}
	// request_id idempotency (DESIGN §7.3): a re-sent confirmation must not
	// re-decide a pending request. The claim is rolled back on every
	// non-accepted path (#7 fix) so a retry after a failure is not
	// swallowed as a duplicate.
	var key string
	accepted := false
	if body.RequestID != "" {
		key = "c:" + body.RequestID
		if g.requestDup(id, key) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}
		defer func() {
			if !accepted {
				g.requestRollback(id, key)
			}
		}()
	}
	allowed := false
	switch body.Decision {
	case "allow":
		allowed = true
	case "deny":
	default:
		writeErr(w, http.StatusBadRequest, `field "decision" must be "allow" or "deny"`)
		return
	}

	g.mu.Lock()
	p, ok := g.confirms[body.ID]
	g.mu.Unlock()
	if !ok || p.sessionID != id {
		writeErr(w, http.StatusNotFound, "confirmation not pending")
		return
	}
	select {
	case p.decision <- core.Confirmation{Allowed: allowed}:
		accepted = true
	default:
		writeErr(w, http.StatusConflict, "confirmation already decided")
		return
	}
	if allowed {
		writeJSON(w, http.StatusOK, map[string]string{"status": "allowed"})
	} else {
		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
	}
}

// handleModel switches the session's model (DESIGN §3.5): it only updates
// the session meta, so a switch mid-run takes effect at the next turn
// boundary without interrupting the in-flight response. The provider must
// be registered in the runner (unknown providers would silently fall back
// to the default, which would mask typos).
func (g *Gateway) handleModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	meta, err := g.sessionFor(ctx, id)
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.Provider) == "" || strings.TrimSpace(body.Model) == "" {
		writeErr(w, http.StatusBadRequest, `fields "provider" and "model" are required`)
		return
	}
	runner, err := g.runnerFor(meta)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	if _, ok := runner.Providers[body.Provider]; !ok {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+body.Provider)
		return
	}
	meta.Provider = body.Provider
	meta.Model = body.Model
	if err := g.store.UpdateSession(ctx, meta); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"provider": meta.Provider, "model": meta.Model})
}

func (g *Gateway) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := g.sessionFor(r.Context(), id); err != nil {
		writeSessionErr(w, err)
		return
	}
	g.mu.Lock()
	cancel, ok := g.cancels[id]
	g.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusConflict, "session is not running")
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// handleResume resumes an interrupted session (DESIGN §6.5): after a
// graceful shutdown or crash left the session interrupted, this restarts the
// loop from the last complete message with no new prompt. Only interrupted
// sessions may be resumed.
func (g *Gateway) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, err := g.sessionFor(r.Context(), id)
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	if meta.Status != session.StatusInterrupted {
		writeErr(w, http.StatusConflict, "session is not interrupted")
		return
	}
	if _, err := g.runnerFor(meta); err != nil {
		writeAgentErr(w, err)
		return
	}
	runID, err := g.startContinue(id)
	if err != nil {
		if err == session.ErrSessionBusy {
			writeErr(w, http.StatusConflict, "session is already running")
			return
		}
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed", "run_id": runID})
}

// handleUsage reports the session's cumulative token usage and cost
// (DESIGN §7.6). The numbers are the per-run aggregates the loop folds into
// the session header, so they survive restarts without re-scanning entries.
func (g *Gateway) handleUsage(w http.ResponseWriter, r *http.Request) {
	meta, err := g.sessionFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tokensIn":  meta.TokensIn,
		"tokensOut": meta.TokensOut,
		"cost":      meta.Cost,
		"provider":  meta.Provider,
		"model":     meta.Model,
	})
}

// handleContextStats reports working-context occupancy (DESIGN §8): current
// tokens, the model window, the occupancy percentage, and how many L2
// compactions have run. The front-end shows this as a "context N% used" bar.
func (g *Gateway) handleContextStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, err := g.sessionFor(r.Context(), id)
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	runner, err := g.runnerFor(meta)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runner.ContextStats(r.Context(), id))
}

// --- helpers ------------------------------------------------------------

// currentUser resolves the request identity: the authenticated user
// from context (stamped via ContextWithUserID), else the
// WithAnonymousUser dev fallback, else "".
func (g *Gateway) currentUser(ctx context.Context) string {
	if u := UserIDFromContext(ctx); u != "" {
		return u
	}
	return g.anonUser
}

// sessionFor loads the session and enforces access control. Identity is
// checked BEFORE the store read: an unauthenticated caller gets 401 for
// every id alike, so the 401/404 split cannot be used to probe which
// session ids exist. Unknown ids and denied access are deliberately
// indistinguishable (both surface as 404).
func (g *Gateway) sessionFor(ctx context.Context, id string) (session.SessionMeta, error) {
	if g.currentUser(ctx) == "" {
		return session.SessionMeta{}, errNoUser
	}
	meta, err := g.store.GetSession(ctx, id)
	if err != nil {
		return session.SessionMeta{}, errForbidden
	}
	if err := g.checkAccess(ctx, meta); err != nil {
		return session.SessionMeta{}, err
	}
	return meta, nil
}

// checkAccess is fail-closed: an identity is required (context user or
// the anonymous dev fallback). The owner check always runs; a custom
// Authorizer runs only after the owner check failed, so it can widen
// access (admins) but never replace or weaken the owner check. The empty
// user branch is defensive — sessionFor already rejected unauthenticated
// callers before the store read.
func (g *Gateway) checkAccess(ctx context.Context, meta session.SessionMeta) error {
	user := g.currentUser(ctx)
	if user == "" {
		return errNoUser
	}
	if meta.UserID == user {
		return nil
	}
	if g.authorizer != nil {
		return g.authorizer(ctx, meta)
	}
	return errForbidden
}

var (
	errNoUser        = errors.New("gateway: no authenticated user")
	errForbidden     = errors.New("gateway: session access denied")
	errAgentRequired = errors.New("agent is required")
	errUnknownAgent  = errors.New("agent runner not registered")
)

// runnerForCreate selects the Runner for a new session. With a nonempty
// agent map the type is mandatory and must already be registered (400).
func (g *Gateway) runnerForCreate(agent string) (*core.Runner, error) {
	if len(g.agents) == 0 {
		return g.runner, nil
	}
	if agent == "" {
		return nil, errAgentRequired
	}
	r, ok := g.agents[agent]
	if !ok {
		return nil, fmt.Errorf("unknown agent: %s", agent)
	}
	return r, nil
}

// runnerFor selects the Runner for an existing session. A registered agent
// map that does not contain meta.Agent is a configuration gap (503), not a
// missing session.
func (g *Gateway) runnerFor(meta session.SessionMeta) (*core.Runner, error) {
	if len(g.agents) == 0 {
		return g.runner, nil
	}
	if r, ok := g.agents[meta.Agent]; ok {
		return r, nil
	}
	return nil, errUnknownAgent
}

func (g *Gateway) runnerForID(id string) (*core.Runner, error) {
	meta, err := g.store.GetSession(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return g.runnerFor(meta)
}

func writeAgentErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnknownAgent) {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

// writeSessionErr maps sessionFor errors to responses: a missing
// identity is 401; everything else (unknown session, denied) is 404.
func writeSessionErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoUser) {
		writeErr(w, http.StatusUnauthorized, "no authenticated user")
		return
	}
	writeErr(w, http.StatusNotFound, "session not found")
}

// sessionUserID is the tenant for a session's in-flight run cap, resolved
// at startLoop where no request context exists: the session owner, else
// "default". Prompt-rate limiting uses currentUser on the HTTP path instead.
func (g *Gateway) sessionUserID(id string) string {
	if meta, err := g.store.GetSession(context.Background(), id); err == nil && meta.UserID != "" {
		return meta.UserID
	}
	return "default"
}

func (g *Gateway) setStatus(ctx context.Context, id string, st session.SessionStatus) {
	meta, err := g.store.GetSession(ctx, id)
	if err != nil {
		return
	}
	meta.Status = st
	_ = g.store.UpdateSession(ctx, meta)
}

func (g *Gateway) releaseCancel(id string) {
	g.mu.Lock()
	delete(g.cancels, id)
	g.mu.Unlock()
}

func writeSSEFrame(w http.ResponseWriter, f sseFrame) {
	fmt.Fprintf(w, "id: %s\nevent: agent\ndata: %s\n\n", f.ID, f.Data)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func newSessionID() string { return newID("s_") }

func newPromptID() string { return newID("p_") }

func newRunID() string { return newID("r_") }

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
