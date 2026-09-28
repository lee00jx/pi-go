package provider

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/lee00jx/pi-go/core"
)

// RetryConfig tunes WithRetry. Zero values fall back to the DESIGN §3.4
// defaults: 3 retries, 500ms base, 8s cap.
type RetryConfig struct {
	// MaxRetries is the number of retries AFTER the first attempt.
	MaxRetries int
	// BaseDelay is the base for the exponential backoff.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff sleep.
	MaxDelay time.Duration

	// Fallback, when non-nil, is invoked after the primary's retryable
	// failures are exhausted. Its request model is replaced by
	// FallbackModel (when non-nil); the done event it produces is tagged
	// with that model so core can emit a model_changed event.
	Fallback      core.StreamFn
	FallbackModel *core.Model
	// OnFallback, when non-nil, is called once when the fallback kicks in
	// (e.g. to log or update per-session state in the integrator).
	OnFallback func(fallbackModel core.Model)
}

// WithRetry wraps a StreamFn with retry + fallback semantics (DESIGN §3.4):
//
//   - transport errors, HTTP 429 / 408 / 5xx: exponential backoff with full
//     jitter, up to MaxRetries;
//   - HTTP 4xx parameter errors and context cancellation: fail fast;
//   - a stream that already produced deltas is never retried (the consumer
//     saw content): the error is surfaced with the partial message so the
//     loop can keep it as history;
//   - after the primary exhausts its retries, Fallback (if set) takes over
//     once, with no retry of its own.
//
// The returned StreamFn never returns a non-nil error: every failure
// arrives as a single StreamError event, so consumers have one path.
func WithRetry(fn core.StreamFn, cfg *RetryConfig) core.StreamFn {
	if cfg == nil {
		cfg = &RetryConfig{}
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	baseDelay := cfg.BaseDelay
	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	maxDelay := cfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 8 * time.Second
	}

	return func(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
		out := make(chan core.StreamEvent, 64)
		go func() {
			defer close(out)
			send := func(ev core.StreamEvent) {
				select {
				case out <- ev:
				case <-ctx.Done():
				}
			}
			emitStart := true // forward the next stream's start event
			emittedContent := false
			usedFallback := false

			// pump forwards one provider stream into out. It reports:
			// true when the stream completed (done event forwarded);
			// false with the terminal StreamError otherwise.
			pump := func(ch <-chan core.StreamEvent, tagModel *core.Model) (completed bool, terminal core.StreamEvent) {
				for ev := range ch {
					select {
					case <-ctx.Done():
						return false, core.StreamEvent{Type: core.StreamError, Err: ctx.Err()}
					default:
					}
					switch ev.Type {
					case core.StreamStart:
						if emitStart {
							send(ev)
							emitStart = false
						}
					case core.StreamTextDelta, core.StreamThinkingDelta, core.StreamToolCallDelta:
						emittedContent = true
						send(ev)
					case core.StreamDone:
						if tagModel != nil && ev.Model == nil {
							ev.Model = tagModel
						}
						send(ev)
						return true, core.StreamEvent{}
					case core.StreamError:
						return false, ev
					}
				}
				return false, core.StreamEvent{Type: core.StreamError, Err: errStreamEndedUnexpectedly}
			}

			attempt := 0
			for {
				ch, err := fn(ctx, req)
				var terminal core.StreamEvent
				if err != nil {
					terminal = core.StreamEvent{Type: core.StreamError, Err: err}
				} else {
					if completed, term := pump(ch, nil); completed {
						return
					} else {
						terminal = term
					}
				}
				if ctx.Err() != nil {
					return
				}

				retryable := IsRetryable(terminal.Err)
				if !retryable || emittedContent || attempt >= maxRetries {
					if attempt >= maxRetries && retryable && cfg.Fallback != nil && !usedFallback {
						usedFallback = true
						fbModel := req.Model
						if cfg.FallbackModel != nil {
							fbModel = *cfg.FallbackModel
						}
						if cfg.OnFallback != nil {
							cfg.OnFallback(fbModel)
						}
						fbReq := req
						fbReq.Model = fbModel
						fbCh, fbErr := cfg.Fallback(ctx, fbReq)
						if fbErr == nil {
							if completed, term := pump(fbCh, &fbModel); completed {
								return
							} else {
								terminal = term
							}
						} else {
							terminal = core.StreamEvent{Type: core.StreamError, Err: fbErr}
						}
					}
					send(terminal)
					return
				}
				attempt++
				// Full jitter over the exponential window (DESIGN §3.4).
				limit := baseDelay << uint(attempt)
				if limit > maxDelay {
					limit = maxDelay
				}
				select {
				case <-time.After(time.Duration(rand.Int64N(int64(limit)))):
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, nil
	}
}

// A channel that closed without done/error is treated as a mid-stream
// disconnect (retryable, same class as a transport EOF).
var errStreamEndedUnexpectedly = errors.New("provider: stream ended without done or error event")
