package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
)

// confirmHooks wires a BeforeToolCall that routes "risky" to confirmation
// (with the hook's risk text) plus a ConfirmTool waiter with the given
// behavior. The fails-closed case (no waiter) builds its own hooks.
func confirmHooks(_ *testing.T, wait func(ctx context.Context, sessionID string, call core.ToolCall) core.Confirmation) core.Hooks {
	return core.Hooks{
		BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
			if call.Name == "risky" {
				return core.Decision{Confirm: true, Reason: "writes to production"}, nil
			}
			return core.Decision{}, nil
		},
		ConfirmTool: func(ctx context.Context, sessionID string, call core.ToolCall) (core.Confirmation, error) {
			return wait(ctx, sessionID, call), nil
		},
	}
}

func confirmOrder(t *testing.T, got []core.AgentEvent, callID string) (risk string, respErr bool, respReason string) {
	t.Helper()
	reqIdx, respIdx, endIdx := -1, -1, -1
	for i, ev := range got {
		switch ev.Type {
		case core.ToolConfirmRequest:
			if ev.ToolCallID == callID {
				reqIdx = i
				risk = ev.Risk
			}
		case core.ToolConfirmResponse:
			if ev.ToolCallID == callID {
				respIdx = i
				respErr = ev.IsError
				respReason = ev.Reason
			}
		case core.ToolExecEnd:
			if ev.ToolCallID == callID {
				endIdx = i
			}
		}
	}
	if reqIdx < 0 || respIdx < 0 || endIdx < 0 {
		t.Fatalf("confirm events missing: req=%d resp=%d end=%d", reqIdx, respIdx, endIdx)
	}
	if !(reqIdx < respIdx && respIdx < endIdx) {
		t.Fatalf("event order = req@%d resp@%d end@%d, want req < resp < end", reqIdx, respIdx, endIdx)
	}
	return risk, respErr, respReason
}

// TestConfirmAllow: a Confirm decision routes to the waiter; on allow the
// tool executes and the events carry the risk + the decision.
func TestConfirmAllow(t *testing.T) {
	risky := &countingTool{name: "risky", mode: core.ModeParallel}
	var gotSession, gotName string
	hooks := confirmHooks(t, func(_ context.Context, sessionID string, call core.ToolCall) core.Confirmation {
		gotSession, gotName = sessionID, call.Name
		return core.Confirmation{Allowed: true}
	})

	got, _ := runScriptWithTools(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "done"},
	}, hooks, risky)

	risky.mu.Lock()
	ran := risky.calls
	risky.mu.Unlock()
	if ran != 1 {
		t.Fatalf("risky executed %d times, want 1 (allowed)", ran)
	}
	if gotSession != "s1" || gotName != "risky" {
		t.Fatalf("waiter saw session=%q call=%q, want s1/risky", gotSession, gotName)
	}
	var callID string
	for _, ev := range got {
		if ev.Type == core.ToolConfirmRequest {
			callID = ev.ToolCallID
		}
	}
	risk, respErr, _ := confirmOrder(t, got, callID)
	if risk != "writes to production" {
		t.Fatalf("risk = %q, want the hook's reason", risk)
	}
	if respErr {
		t.Fatal("response marked as error, want allowed")
	}
}

// TestConfirmDeny: a denial must NOT execute the tool, feeds an error
// result back to the model, and the loop keeps running (DESIGN §11 阶段 3
// 验收:拒绝后模型收到 error 结果继续).
func TestConfirmDeny(t *testing.T) {
	risky := &countingTool{name: "risky", mode: core.ModeParallel}
	hooks := confirmHooks(t, func(context.Context, string, core.ToolCall) core.Confirmation {
		return core.Confirmation{Allowed: false, Reason: "not allowed now"}
	})

	got, _ := runScriptWithTools(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "done"},
	}, hooks, risky)

	risky.mu.Lock()
	ran := risky.calls
	risky.mu.Unlock()
	if ran != 0 {
		t.Fatalf("denied tool executed %d times, want 0", ran)
	}
	var callID string
	for _, ev := range got {
		if ev.Type == core.ToolConfirmRequest {
			callID = ev.ToolCallID
		}
	}
	_, respErr, respReason := confirmOrder(t, got, callID)
	if !respErr || respReason != "not allowed now" {
		t.Fatalf("response = err %v reason %q, want denied with the reason", respErr, respReason)
	}
	// The denied result reached the store as an error tool_result, and the
	// loop continued to the next turn.
	var sawDenyResult, sawFinalText bool
	for _, ev := range got {
		if ev.Type == core.ToolExecEnd && ev.ToolCallID == callID && ev.IsError &&
			ev.Result != nil && strings.Contains(ev.Result.Output, "not allowed now") {
			sawDenyResult = true
		}
		if ev.Type == core.MessageEnd && ev.Message != nil && ev.Message.Text() == "done" {
			sawFinalText = true
		}
	}
	if !sawDenyResult || !sawFinalText {
		t.Fatalf("deny result=%v loop continued=%v, want both", sawDenyResult, sawFinalText)
	}
}

// TestConfirmTimeout: the waiter's own timeout (DESIGN §5.3: default 120s,
// timeout = deny) must come back as a denial, not a hang.
func TestConfirmTimeout(t *testing.T) {
	risky := &countingTool{name: "risky", mode: core.ModeParallel}
	hooks := confirmHooks(t, func(ctx context.Context, _ string, _ core.ToolCall) core.Confirmation {
		select {
		case <-time.After(150 * time.Millisecond):
			return core.Confirmation{Allowed: false, Reason: "confirmation timed out"}
		case <-ctx.Done():
			return core.Confirmation{Allowed: false, Reason: "run stopped"}
		}
	})

	got, _ := runScriptWithTools(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "done"},
	}, hooks, risky)

	risky.mu.Lock()
	ran := risky.calls
	risky.mu.Unlock()
	if ran != 0 {
		t.Fatalf("timed-out tool executed %d times, want 0", ran)
	}
	var callID string
	for _, ev := range got {
		if ev.Type == core.ToolConfirmRequest {
			callID = ev.ToolCallID
		}
	}
	_, respErr, respReason := confirmOrder(t, got, callID)
	if !respErr || respReason != "confirmation timed out" {
		t.Fatalf("response = err %v reason %q, want timeout denial", respErr, respReason)
	}
}

// TestConfirmFailsClosed: a Confirm decision with no ConfirmTool hook
// wired must be denied, never executed — and never silently allowed.
func TestConfirmFailsClosed(t *testing.T) {
	risky := &countingTool{name: "risky", mode: core.ModeParallel}
	hooks := core.Hooks{
		BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
			if call.Name == "risky" {
				return core.Decision{Confirm: true}, nil
			}
			return core.Decision{}, nil
		},
		// ConfirmTool deliberately nil.
	}

	got, _ := runScriptWithTools(t, []provider.FauxTurn{
		{ToolCalls: []provider.FauxToolCall{{Name: "risky"}}},
		{Text: "done"},
	}, hooks, risky)

	risky.mu.Lock()
	ran := risky.calls
	risky.mu.Unlock()
	if ran != 0 {
		t.Fatalf("unhandled confirm executed the tool %d times, want 0", ran)
	}
	var callID string
	for _, ev := range got {
		if ev.Type == core.ToolConfirmRequest {
			callID = ev.ToolCallID
		}
	}
	_, _, respReason := confirmOrder(t, got, callID)
	if !strings.Contains(respReason, "no confirmation handler") {
		t.Fatalf("reason = %q, want the fails-closed explanation", respReason)
	}
}
