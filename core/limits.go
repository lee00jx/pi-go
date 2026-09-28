package core

import (
	"context"
	"log/slog"
	"time"
)

// scopedContext returns a context limited to d (when d > 0), or a plain
// cancellable child otherwise. The caller must call the returned cancel once
// the scoped work completes. Using WithCancel for the zero (unlimited) case
// keeps one code path for both settings.
func scopedContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

// costOf prices one assistant message's usage (DESIGN §7.6). It returns 0
// when no pricing function is wired or the message carries no usage, so a
// deployment without a price table still tracks token counts (cost stays 0).
func (r *Runner) costOf(m Model, u *Usage) float64 {
	if u == nil || r.CostPerMillion == nil {
		return 0
	}
	inPrice, outPrice := r.CostPerMillion(m)
	return float64(u.InputTokens)/1e6*inPrice + float64(u.OutputTokens)/1e6*outPrice
}

// persistRunUsage folds one run's usage into the session header (DESIGN
// §7.6) so GET /usage can answer without re-scanning every entry. It is a
// no-op when the run produced no usage. The caller MUST pass
// context.Background() (not the run's ctx): a graceful shutdown cancels
// the run ctx, and using it here would silently drop the usage write,
// losing the TokenBudget baseline-across-restart guarantee (ADR-017).
func (r *Runner) persistRunUsage(ctx context.Context, sessionID string, tokensIn, tokensOut int64, cost float64, log *slog.Logger) {
	if r.Store == nil || (tokensIn == 0 && tokensOut == 0 && cost == 0) {
		return
	}
	meta, err := r.Store.GetSession(ctx, sessionID)
	if err != nil {
		if !isContextStop(err) {
			log.Warn("usage: get session", "err", err)
		}
		return
	}
	meta.TokensIn += tokensIn
	meta.TokensOut += tokensOut
	meta.Cost += cost
	if err := r.Store.UpdateSession(ctx, meta); err != nil {
		if !isContextStop(err) {
			log.Warn("usage: update session", "err", err)
		}
	}
}
