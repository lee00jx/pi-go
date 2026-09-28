package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lee00jx/pi-go/session"
)

// isContextStop reports whether err is the run's context being cancelled or
// hitting its deadline — the expected, non-error way a run stops (graceful
// shutdown, POST /stop). Callers use it to keep the stop path quiet.
func isContextStop(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// TurnSnapshot is handed to per-turn hooks at the turn boundary.
type TurnSnapshot struct {
	Turn        int
	Message     *Message
	ToolResults []*Message
	// Messages is the working context actually fed to the LLM this turn.
	Messages []*Message
}

// NextTurnConfig is what PrepareNextTurn may change for the next turn.
// Zero fields mean "keep the current value".
type NextTurnConfig struct {
	Model         *Model
	SystemPrompt  string
	ThinkingLevel string
}

// Hooks are the extension points (DESIGN §2.4). All are optional.
type Hooks struct {
	// BeforeToolCall may block a call or route it to Web confirmation.
	// This is also where integrators enforce tool-level permissions
	// (e.g. Casbin in the hook, ADR-004).
	BeforeToolCall func(ctx context.Context, call ToolCall) (Decision, error)

	// ConfirmTool is called (after tool_confirmation_request was emitted)
	// when BeforeToolCall answers Confirm. It must block until the user
	// decides, and carry its own timeout (DESIGN §5.3: default 120s,
	// timeout = deny). When unset, a Confirm decision fails closed:
	// the call is denied with a reason, never executed.
	// Carries sessionID because one Runner serves many sessions.
	ConfirmTool func(ctx context.Context, sessionID string, call ToolCall) (Confirmation, error)

	// AfterToolCall may rewrite / truncate a result before it is fed back.
	// Concurrency: a parallel tool batch invokes it once per call
	// concurrently — the hook must be safe for concurrent use.
	AfterToolCall func(ctx context.Context, call ToolCall, result ToolResult) ToolResult

	// PrepareNextTurn runs at turn boundaries only (model switch,
	// compaction, thinking level) — never mid tool execution, so
	// toolCall/toolResult pairs stay intact (DESIGN §6.4).
	PrepareNextTurn func(ctx context.Context, snap TurnSnapshot) (NextTurnConfig, error)

	// ShouldStopAfterTurn enforces max turns / token budgets (phase 5).
	ShouldStopAfterTurn func(ctx context.Context, snap TurnSnapshot) bool

	// TransformContext is the last working-context transform before the LLM
	// call (phase 5: L1 tool-result eviction hooks in here).
	TransformContext func(ctx context.Context, msgs []*Message) ([]*Message, error)

	// GetSteeringMessages: user messages typed while running; injected at
	// the next turn boundary (DESIGN §2.5). It must DRAIN (return and
	// clear) its queue, since the loop calls it once per turn boundary.
	// Carries sessionID because one Runner serves many sessions.
	GetSteeringMessages func(ctx context.Context, sessionID string) []*Message

	// GetFollowUpMessages: extra messages when the loop is about to stop.
	GetFollowUpMessages func(ctx context.Context, sessionID string) []*Message
}

// ProviderBinding couples one provider name to its StreamFn and the model
// metadata for that provider (context window, default model id).
type ProviderBinding struct {
	StreamFn StreamFn
	Model    Model
}

// Runner carries everything one agent deployment needs. A gateway (or any
// host) builds one Runner and starts runs against session IDs.
type Runner struct {
	Store    session.Store
	StreamFn StreamFn // default when the session names no provider
	Tools    []Tool
	Hooks    Hooks
	// Model is the default model (used when the session's provider has no
	// binding). Sessions may name another provider/model (DESIGN §3.5).
	Model Model
	// Providers maps provider name → binding for per-session model
	// selection. Resolved from the session's meta at run start and re-
	// checked at every turn boundary, so a model switch (gateway
	// POST /model) takes effect at the next turn without a restart.
	Providers    map[string]ProviderBinding
	SystemPrompt string
	MaxTurns     int // 0 = unlimited
	// Resource limits (DESIGN §7.4). All zero = no limit.
	TurnTimeout time.Duration // per-LLM-turn deadline; 0 = unlimited
	ToolTimeout time.Duration // per-tool Execute deadline; 0 = unlimited
	// TokenBudget caps the session's cumulative (tokens_in + tokens_out);
	// exceeding it emits budget_exhausted and stops the loop after the
	// current assistant message. 0 = unlimited.
	TokenBudget int
	// Sampling is copied onto every LLM turn (and left zero on the L2
	// summarizer, which should stay on server defaults). Zero value =
	// omit temperature / top_p / max_tokens / seed — the integrator who
	// is happy with vLLM/OpenAI defaults fills nothing.
	Sampling Sampling
	// Compaction configures two-level context compaction (DESIGN §6.4). The
	// zero value disables it (pure replay); use DefaultCompaction() for the
	// DESIGN defaults.
	Compaction CompactionConfig
	// CostPerMillion prices a model for cost accounting (DESIGN §7.6); it
	// returns (inputPrice, outputPrice) per million tokens in the
	// deployment's currency. nil = cost stays 0 (usage is still tracked).
	CostPerMillion func(Model) (in, out float64)
	Logger         *slog.Logger
}

// streamFnFor picks the StreamFn that serves the given model: the provider
// binding when one is registered, the runner default otherwise.
func (r *Runner) streamFnFor(m Model) StreamFn {
	if b, ok := r.Providers[m.Provider]; ok && b.StreamFn != nil {
		return b.StreamFn
	}
	return r.StreamFn
}

// resolveSessionModel maps the session's stored (provider, model) to a
// concrete Model (DESIGN §3.5). ok=false means "keep the default": no
// store, session has no provider, or the provider has no binding (a
// warning is logged in that last case — the run continues on the default
// rather than failing the session).
func (r *Runner) resolveSessionModel(ctx context.Context, sessionID string) (Model, bool) {
	if r.Store == nil || len(r.Providers) == 0 {
		return Model{}, false
	}
	meta, err := r.Store.GetSession(ctx, sessionID)
	if err != nil || meta.Provider == "" {
		return Model{}, false
	}
	b, ok := r.Providers[meta.Provider]
	if !ok {
		r.log().Warn("session provider has no binding, using default",
			"session", sessionID, "provider", meta.Provider)
		return Model{}, false
	}
	m := b.Model
	if meta.Model != "" {
		m.ID = meta.Model
	}
	return m, true
}

// Run starts the ReAct loop (DESIGN §2.1) for sessionID with the given user
// prompts. It returns immediately; events arrive on the channel until it
// closes.
//
// The loop does not depend on any client connection: consumers may drop
// without stopping it. Event persistence is the caller's job (the gateway
// appends every event as a ui_event entry and re-broadcasts it).
func (r *Runner) Run(ctx context.Context, sessionID string, prompts []*Message) (<-chan AgentEvent, error) {
	if r.StreamFn == nil {
		return nil, errors.New("core: Runner.StreamFn is required")
	}
	if len(prompts) == 0 {
		return nil, errors.New("core: Run requires at least one prompt")
	}
	out := make(chan AgentEvent, 64)
	go func() {
		defer close(out)
		r.run(ctx, sessionID, prompts, out)
	}()
	return out, nil
}

// RunContinue resumes an interrupted session (DESIGN §6.5, agentLoopContinue
// semantics): no new user prompt — the loop rebuilds the working context
// from the store and asks the model to continue from the last complete
// message. A mid-turn interruption can leave the trailing assistant message
// with tool calls whose results were never persisted; providers reject such
// dangling calls, so repairDanglingToolCalls synthesizes error results for
// them before the loop starts.
func (r *Runner) RunContinue(ctx context.Context, sessionID string) (<-chan AgentEvent, error) {
	if r.StreamFn == nil {
		return nil, errors.New("core: Runner.StreamFn is required")
	}
	if r.Store == nil {
		return nil, errors.New("core: Runner.Store is required for RunContinue")
	}
	if _, err := r.Store.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	if err := r.repairDanglingToolCalls(ctx, sessionID); err != nil {
		return nil, err
	}
	if _, msgs, err := r.buildContext(ctx, sessionID); err != nil || len(msgs) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("core: session has no history to continue")
	}
	out := make(chan AgentEvent, 64)
	go func() {
		defer close(out)
		r.run(ctx, sessionID, nil, out)
	}()
	return out, nil
}

// repairDanglingToolCalls appends synthesized error tool results for any
// tool call on the trailing assistant message that has no matching tool
// result (a mid-turn interruption). It is a no-op when the history already
// ends cleanly. Only the trailing assistant message can carry dangling
// calls in a well-formed append-only history, and the append-only store
// means a fix is only expressible there — so we look only at the tail.
func (r *Runner) repairDanglingToolCalls(ctx context.Context, sessionID string) error {
	_, msgs, err := r.buildContext(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != RoleAssistant {
		return nil
	}
	calls := last.ToolCalls()
	if len(calls) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleTool && m.ToolCallID != "" {
			have[m.ToolCallID] = struct{}{}
		}
	}
	for _, c := range calls {
		if _, ok := have[c.ID]; ok {
			continue
		}
		res := NewToolResultMessage(c, ToolResult{
			Output:  "tool execution was interrupted before it completed",
			IsError: true,
		})
		res.PromptID = last.PromptID
		if res.PromptID == "" {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == RoleUser && msgs[i].PromptID != "" {
					res.PromptID = msgs[i].PromptID
					break
				}
			}
		}
		payload, _ := json.Marshal(res)
		if _, err := r.Store.AppendEntry(ctx, sessionID, session.EntryToolResult, payload, nil); err != nil {
			return err
		}
	}
	return nil
}

// Hook accessors guard against zero-value Hooks (all function fields are
// optional).

func (r *Runner) steering(ctx context.Context, sessionID string) []*Message {
	if r.Hooks.GetSteeringMessages != nil {
		return r.Hooks.GetSteeringMessages(ctx, sessionID)
	}
	return nil
}

func (r *Runner) followUp(ctx context.Context, sessionID string) []*Message {
	if r.Hooks.GetFollowUpMessages != nil {
		return r.Hooks.GetFollowUpMessages(ctx, sessionID)
	}
	return nil
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) run(ctx context.Context, sessionID string, prompts []*Message, out chan<- AgentEvent) {
	log := r.log().With("session", sessionID)
	// emit never blocks the loop on a dead subscriber: the gateway persists
	// events to the store independently of who is listening on the channel.
	emit := func(ev AgentEvent) {
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}
	var activePromptID string
	for _, p := range prompts {
		if p != nil && p.PromptID != "" {
			activePromptID = p.PromptID
		}
	}
	appendMsg := func(m *Message) {
		if m == nil {
			return
		}
		// Copy before stamping PromptID: the live pointer may already be
		// sitting in the event channel (stampRunIDs reads Message.PromptID).
		stored := *m
		if stored.Role == RoleUser && stored.PromptID != "" {
			activePromptID = stored.PromptID
		}
		if stored.PromptID == "" && (stored.Role == RoleAssistant || stored.Role == RoleTool) {
			stored.PromptID = activePromptID
		}
		if r.Store == nil {
			return
		}
		typ := session.EntryUser
		switch stored.Role {
		case RoleAssistant:
			typ = session.EntryAssistant
		case RoleTool:
			typ = session.EntryToolResult
		}
		payload, _ := json.Marshal(&stored)
		var usage json.RawMessage
		if stored.Usage != nil {
			usage, _ = json.Marshal(stored.Usage)
		}
		if _, err := r.Store.AppendEntry(ctx, sessionID, typ, payload, usage); err != nil {
			// A cancelled run (graceful shutdown, POST /stop) makes the
			// trailing appends fail by design — expected, not worth a warning.
			if !isContextStop(err) {
				log.Warn("append entry failed", "err", err)
			}
		}
	}

	newMessages := make([]*Message, 0, 8)
	emit(EvAgentStart())
	for _, p := range prompts {
		emit(EvMessageStart(p))
		emit(EvMessageEnd(p))
		appendMsg(p)
		newMessages = append(newMessages, p)
	}

	// Resource governance (DESIGN §7.4/§7.6): track this run's usage so it
	// can be folded into the session header at the end, and enforce the
	// per-session token budget. The baseline is the stored aggregate, so the
	// budget — and the final numbers — survive restarts.
	var baseIn, baseOut int64
	if r.Store != nil {
		if meta, err := r.Store.GetSession(ctx, sessionID); err == nil {
			baseIn, baseOut = meta.TokensIn, meta.TokensOut
		}
	}
	var runIn, runOut int
	var runCost float64
	overBudget := func() bool {
		return r.TokenBudget > 0 && baseIn+baseOut+int64(runIn+runOut) > int64(r.TokenBudget)
	}
	// Defer a closure so the counters are read at run end (a bare
	// `defer f(runIn, ...)` would capture their zero values now). The
	// persist call uses context.Background(), NOT the run's ctx: Shutdown()
	// cancels the run ctx, and using it here would make GetSession/
	// UpdateSession fail silently via isContextStop — losing this run's
	// usage from the session aggregate and breaking the TokenBudget
	// baseline-across-restart guarantee (ADR-017). setStatus already does
	// the same (uses Background on the shutdown path).
	defer func() {
		r.persistRunUsage(context.Background(), sessionID, int64(runIn), int64(runOut), runCost, log)
	}()

	model := r.Model
	if m, ok := r.resolveSessionModel(ctx, sessionID); ok {
		model = m
	}
	system := r.SystemPrompt
	thinkingLevel := ""
	turn := 0
	var lastSnap TurnSnapshot
	pending := r.steering(ctx, sessionID)

stop:
	for {
		hasToolCalls := true

		for hasToolCalls || len(pending) > 0 {
			// Token budget (DESIGN §7.4): if the session already burned its
			// budget (including prior runs), stop before spending a turn.
			if overBudget() {
				log.Warn("token budget exhausted, stopping",
					"tokens", baseIn+baseOut+int64(runIn+runOut), "budget", r.TokenBudget)
				emit(EvBudgetExhausted())
				break stop
			}

			// Turn-boundary preparation: model switch / compaction /
			// thinking level. Runs only between turns (DESIGN §6.4).
			hookModel := false
			if lastSnap.Message != nil && r.Hooks.PrepareNextTurn != nil {
				// On error keep the current config and continue the turn —
				// a failed compaction/model-switch must never wedge the loop.
				if cfg, err := r.Hooks.PrepareNextTurn(ctx, lastSnap); err != nil {
					log.Warn("prepareNextTurn failed", "err", err)
				} else {
					if cfg.Model != nil {
						// The hook's explicit model wins over the
						// session-level one below.
						hookModel = true
						if cfg.Model.String() != model.String() {
							model = *cfg.Model
							emit(EvModelChanged(model))
						} else {
							model = *cfg.Model
						}
					}
					if cfg.SystemPrompt != "" {
						system = cfg.SystemPrompt
					}
					if cfg.ThinkingLevel != "" {
						thinkingLevel = cfg.ThinkingLevel
					}
				}
			}
			if !hookModel {
				// Session-level model switch (DESIGN §3.5): a POST /model
				// writes the new (provider, model) into the session meta;
				// pick it up here so it takes effect at the very next turn,
				// without interrupting the in-flight response.
				if m, ok := r.resolveSessionModel(ctx, sessionID); ok && m.String() != model.String() {
					model = m
					emit(EvModelChanged(model))
				}
			}

			// L2 compaction at the turn boundary (DESIGN §6.4): L1 already
			// applied in buildContext; if the context still exceeds the
			// window headroom, summarize the older portion. No-op unless
			// compaction is enabled with a Summarize wired.
			r.maybeCompact(ctx, sessionID, model, emit)

			turn++
			emit(EvTurnStart(turn))
			tlog := log.With("turn", turn)

			// Inject steering messages before the next assistant response.
			for _, m := range pending {
				emit(EvMessageStart(m))
				emit(EvMessageEnd(m))
				appendMsg(m)
				newMessages = append(newMessages, m)
			}
			pending = nil

			// Working context is derived from the store every turn.
			// Phase 5: compaction summary splice + L1 eviction apply here.
			// The summary is folded into the system prompt (NOT a standalone
			// user message) to avoid consecutive-user-messages on Anthropic
			// (#1 fix).
			//
			// Fail-closed on store errors (#6 fix): a failed read aborts the
			// turn instead of silently asking the model to continue from
			// an empty context. Context-stop errors take the same path —
			// the run is ending anyway.
			compSummary, msgs, err := r.buildContext(ctx, sessionID)
			if err != nil {
				emit(EvTurnEnd(turn, nil, nil))
				emit(EvAgentEnd(newMessages))
				return
			}
			if r.Hooks.TransformContext != nil {
				if t, err := r.Hooks.TransformContext(ctx, msgs); err == nil {
					msgs = t
				}
			}

			// Per-turn deadline (DESIGN §7.4): one LLM response must not hang
			// the loop; the provider closes its channel when the ctx fires.
			llmCtx, llmCancel := scopedContext(ctx, r.TurnTimeout)
			effectiveSystem := system
			if compSummary != "" {
				effectiveSystem = system + "\n\n<conversation-summary>\n" + compSummary + "\n</conversation-summary>"
			}
			msg, usedModel := r.streamAssistant(llmCtx, Request{
				Model:         model,
				SystemPrompt:  effectiveSystem,
				Messages:      msgs,
				Tools:         r.Tools,
				ThinkingLevel: thinkingLevel,
				Sampling:      r.Sampling,
			}, emit)
			llmCancel()
			if usedModel != nil && usedModel.String() != model.String() {
				// Provider-level fallback (DESIGN §3.4): subsequent turns
				// use the model that actually answered.
				model = *usedModel
				emit(EvModelChanged(model))
			}
			if msg == nil {
				// Aborted or fatal provider error: the provider side already
				// surfaced an error event (or ctx is done).
				emit(EvTurnEnd(turn, nil, nil))
				emit(EvAgentEnd(newMessages))
				return
			}
			newMessages = append(newMessages, msg)
			appendMsg(msg)

			// Usage + cost accounting (DESIGN §7.6): only assistant messages
			// carry usage. Accumulate for the run-end persist and the budget.
			budgetHit := false
			if msg.Usage != nil {
				runIn += msg.Usage.InputTokens
				runOut += msg.Usage.OutputTokens
				runCost += r.costOf(model, msg.Usage)
			}
			if overBudget() {
				budgetHit = true
				log.Warn("token budget exhausted, stopping after this turn",
					"turn", turn, "tokens", baseIn+baseOut+int64(runIn+runOut), "budget", r.TokenBudget)
			}

			hasToolCalls = false
			var toolResults []*Message
			if calls := msg.ToolCalls(); len(calls) > 0 && !budgetHit {
				var allTerminate bool
				if msg.StopReason == StopLength || msg.StopReason == StopError {
					// Truncation protection (DESIGN §2.3, pi:379): arguments of
					// a truncated response (output limit OR interrupted
					// stream) may salvage-parse but are silently incomplete —
					// none of them may be executed.
					reason := "the response hit the output token limit"
					if msg.StopReason == StopError {
						reason = "the response was interrupted before completion"
					}
					toolResults, allTerminate = r.failTruncatedToolCalls(calls, reason, emit, appendMsg)
				} else {
					toolResults, allTerminate = r.executeTools(ctx, sessionID, calls, emit, appendMsg)
				}
				newMessages = append(newMessages, toolResults...)
				hasToolCalls = !allTerminate
			}

			lastSnap = TurnSnapshot{Turn: turn, Message: msg, ToolResults: toolResults, Messages: msgs}
			emit(EvTurnEnd(turn, msg, toolResults))
			if budgetHit {
				// Force-stop on budget (DESIGN §7.4); the turn above ended
				// cleanly, so the event stream stays balanced.
				emit(EvBudgetExhausted())
				break stop
			}

			if r.Hooks.ShouldStopAfterTurn != nil && r.Hooks.ShouldStopAfterTurn(ctx, lastSnap) {
				break stop
			}
			if r.MaxTurns > 0 && turn >= r.MaxTurns {
				tlog.Info("max turns reached, stopping", "turns", turn)
				break stop
			}
			pending = r.steering(ctx, sessionID)
		}

		// The agent would stop here; check for queued follow-up messages.
		if fu := r.followUp(ctx, sessionID); len(fu) > 0 {
			pending = fu
			continue
		}
		break
	}

	emit(EvAgentEnd(newMessages))
}

// streamAssistant runs one LLM turn, translating normalized stream events
// into agent events. It returns the final assistant message (nil when the
// turn was aborted / errored with nothing produced) and the model the
// provider actually used when it differs from req.Model (fallback,
// DESIGN §3.4).
func (r *Runner) streamAssistant(ctx context.Context, req Request, emit func(AgentEvent)) (*Message, *Model) {
	// Per-model StreamFn selection (DESIGN §3.5): a mid-run model switch
	// on a different provider routes the next turn to the right client.
	ch, err := r.streamFnFor(req.Model)(ctx, req)
	if err != nil {
		// Retry/backoff/fallback live in the provider layer (WithRetry);
		// by the time we see an error here it is terminal for this turn.
		r.log().Error("stream start failed", "model", req.Model.String(), "err", err)
		return nil, nil
	}

	var final *Message
	var usedModel *Model
	started := false
	for ev := range ch {
		switch ev.Type {
		case StreamStart:
			started = true
			final = ev.Message
			emit(EvMessageStart(ev.Message))
		case StreamTextDelta:
			emit(EvTextDelta(ev.Text))
		case StreamThinkingDelta:
			emit(EvThinkingDelta(ev.Text))
		case StreamToolCallDelta:
			emit(EvToolCallDelta(ev.Text, ev.ToolCallID, ev.ToolName))
		case StreamDone:
			if ev.Message != nil {
				final = ev.Message
			}
			usedModel = ev.Model
		case StreamError:
			r.log().Error("stream error", "model", req.Model.String(), "err", ev.Err)
			// Keep any partial output as history (DESIGN §3.4): the model
			// sees the cut-off answer and the loop ends this turn.
			if ev.Message != nil {
				final = ev.Message
				final.StopReason = StopError
				if !started {
					emit(EvMessageStart(final))
				}
				emit(EvMessageEnd(final))
				return final, ev.Model
			}
			return nil, ev.Model
		}
	}
	if final == nil {
		return nil, nil
	}
	if !started {
		emit(EvMessageStart(final))
	}
	emit(EvMessageEnd(final))
	return final, usedModel
}

// failTruncatedToolCalls turns every tool call of a truncated assistant
// message into an error result without executing it (pi:
// failToolCallsFromTruncatedMessage). reason states why (output token
// limit or interrupted stream); the model is told to re-issue the calls
// with complete arguments.
func (r *Runner) failTruncatedToolCalls(
	calls []ToolCall,
	reason string,
	emit func(AgentEvent),
	appendMsg func(*Message),
) ([]*Message, bool) {
	r.log().Warn("response truncated; failing tool batch without execution", "reason", reason, "calls", len(calls))
	results := make([]*Message, 0, len(calls))
	for _, call := range calls {
		emit(EvToolExecStart(call))
		res := ToolResult{
			Output: fmt.Sprintf(
				`Tool call %q was not executed: %s, so its arguments may be truncated. Re-issue the tool call with complete arguments.`,
				call.Name, reason,
			),
			IsError: true,
		}
		msg := NewToolResultMessage(call, res)
		appendMsg(msg)
		results = append(results, msg)
		emit(EvToolExecEnd(call, res))
	}
	return results, false
}

// executeTools runs a batch of tool calls. A batch containing any
// sequential-mode tool runs entirely in order (pi semantics: one
// sequential tool downgrades the whole batch); otherwise the calls run in
// parallel and tool-result entries are persisted in the original call
// order. Returns the tool-result messages and whether every result
// requested termination.
func (r *Runner) executeTools(
	ctx context.Context,
	sessionID string,
	calls []ToolCall,
	emit func(AgentEvent),
	appendMsg func(*Message),
) ([]*Message, bool) {
	sequential := false
	for _, call := range calls {
		if t := r.findTool(call.Name); t != nil && t.ExecutionMode() == ModeSequential {
			sequential = true
			break
		}
	}

	results := make([]*Message, 0, len(calls))
	if sequential {
		return r.executeToolsSequential(ctx, sessionID, calls, emit, appendMsg)
	}

	// Parallel: announce every call, run them concurrently, then persist and
	// report results in the original call order (pi: Promise.all + ordered
	// mapping).
	for _, call := range calls {
		emit(EvToolExecStart(call))
	}
	type outcome struct {
		call ToolCall
		res  ToolResult
	}
	outcomes := make([]outcome, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call ToolCall) {
			defer wg.Done()
			res := r.runOneTool(ctx, sessionID, call, emit)
			if r.Hooks.AfterToolCall != nil {
				res = r.Hooks.AfterToolCall(ctx, call, res)
			}
			outcomes[i] = outcome{call: call, res: res}
			emit(EvToolExecEnd(call, res))
		}(i, call)
	}
	wg.Wait()

	allTerminate := true
	for _, o := range outcomes {
		// The store keeps the raw result; the boundary marker that frames
		// tool output as untrusted data is applied at buildContext time
		// (compaction.go wrapToolData), not at the store-write path.
		msg := NewToolResultMessage(o.call, o.res)
		appendMsg(msg)
		results = append(results, msg)
		if !o.res.Terminate {
			allTerminate = false
		}
	}
	return results, allTerminate
}

// executeToolsSequential runs calls one by one, persisting and reporting
// each as it finishes.
func (r *Runner) executeToolsSequential(
	ctx context.Context,
	sessionID string,
	calls []ToolCall,
	emit func(AgentEvent),
	appendMsg func(*Message),
) ([]*Message, bool) {
	results := make([]*Message, 0, len(calls))
	allTerminate := true
	for _, call := range calls {
		if ctx.Err() != nil {
			// Aborted: drop the remaining calls; the partial turn is kept.
			allTerminate = false
			break
		}
		emit(EvToolExecStart(call))
		res := r.runOneTool(ctx, sessionID, call, emit)
		if r.Hooks.AfterToolCall != nil {
			res = r.Hooks.AfterToolCall(ctx, call, res)
		}
		msg := NewToolResultMessage(call, res)
		appendMsg(msg)
		results = append(results, msg)
		emit(EvToolExecEnd(call, res))
		if !res.Terminate {
			allTerminate = false
		}
	}
	if len(results) == 0 {
		allTerminate = false
	}
	return results, allTerminate
}

// runOneTool resolves hooks (permission/confirmation) and executes one call.
// Missing tools and validation failures come back as error results so the
// model can self-correct without breaking the loop (DESIGN §2.3).
func (r *Runner) runOneTool(ctx context.Context, sessionID string, call ToolCall, emit func(AgentEvent)) ToolResult {
	if ctx.Err() != nil {
		// Aborted before this call even started (parallel path).
		return ToolResult{Output: "Operation aborted", IsError: true}
	}
	tool := r.findTool(call.Name)
	if tool == nil {
		return ToolResult{Output: fmt.Sprintf("Tool %s not found", call.Name), IsError: true}
	}
	if r.Hooks.BeforeToolCall != nil {
		d, err := r.Hooks.BeforeToolCall(ctx, call)
		if err != nil {
			return ToolResult{Output: err.Error(), IsError: true}
		}
		if d.Block {
			reason := d.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return ToolResult{Output: reason, IsError: true, Terminate: d.Terminate}
		}
		if d.Confirm {
			// Web confirmation (DESIGN §5.3): announce, wait on the
			// ConfirmTool hook (which owns its timeout), then allow or
			// deny. A denial is an error result the model can react to —
			// the loop keeps running.
			emit(EvToolConfirmRequest(call, d.Reason))
			conf, err := r.confirmTool(ctx, sessionID, call)
			if err != nil {
				return ToolResult{Output: err.Error(), IsError: true}
			}
			emit(EvToolConfirmResponse(call, conf))
			if !conf.Allowed {
				reason := conf.Reason
				if reason == "" {
					reason = "the user declined to confirm"
				}
				return ToolResult{
					Output:  fmt.Sprintf("Tool execution was not confirmed: %s. Do not retry the same call.", reason),
					IsError: true,
				}
			}
			if ctx.Err() != nil {
				// The run was stopped while the user was deciding.
				return ToolResult{Output: "Operation aborted", IsError: true}
			}
		}
	}
	// Per-tool deadline (DESIGN §7.4): a hung tool must not wedge the run.
	execCtx, execCancel := scopedContext(ctx, r.ToolTimeout)
	update := func(u ToolUpdate) { emit(EvToolExecUpdate(call, u)) }
	var (
		res ToolResult
		err error
	)
	// SessionTool tools get the session id (DESIGN §6.4 recall_event); plain
	// tools keep the original Execute path — the dispatch is non-breaking.
	if st, ok := tool.(SessionTool); ok {
		res, err = st.ExecuteIn(execCtx, sessionID, call, update)
	} else {
		res, err = tool.Execute(execCtx, call, update)
	}
	execCancel()
	if err != nil {
		if r.ToolTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{
				Output: fmt.Sprintf("tool execution timed out after %s", r.ToolTimeout), IsError: true,
			}
		}
		return ToolResult{Output: err.Error(), IsError: true}
	}
	return res
}

// confirmTool invokes the ConfirmTool hook; without one, a Confirm decision
// fails closed (deny, never execute) — an unhandled "confirm" must never
// silently become an allow.
func (r *Runner) confirmTool(ctx context.Context, sessionID string, call ToolCall) (Confirmation, error) {
	if r.Hooks.ConfirmTool == nil {
		return Confirmation{Allowed: false, Reason: "no confirmation handler is configured"}, nil
	}
	return r.Hooks.ConfirmTool(ctx, sessionID, call)
}

func (r *Runner) findTool(name string) Tool {
	for _, t := range r.Tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// (buildContext moved to compaction.go: it now splices the latest
// compaction summary and applies L1 eviction, per DESIGN §6.3/§6.4.)
