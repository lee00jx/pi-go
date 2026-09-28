package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCharTokens(t *testing.T) {
	// 4 ASCII chars/token (rounded up): "abcd"=1, "abcde"=2, ""=0.
	cases := map[string]int{
		"":      0,
		"abcd":  1,
		"abcde": 2,
		"ab":    1,
	}
	for in, want := range cases {
		if got := charTokens(in); got != want {
			t.Errorf("charTokens(%q) = %d, want %d", in, got, want)
		}
	}
	// Non-ASCII is 1 char/token (Chinese weighted high).
	if got := charTokens("你好"); got != 2 {
		t.Errorf("charTokens(你好) = %d, want 2", got)
	}
	// Mixed: 6 ASCII + 2 CJK = ceil(6/4)+2 = 2+2 = 4.
	if got := charTokens("hello 你好"); got != 4 {
		t.Errorf("charTokens(hello 你好) = %d, want 4", got)
	}
}

// A same-length Chinese string must estimate higher than an ASCII one —
// this is the whole point of the Chinese weighting (a uniform chars/4
// under-counts CJK by ~4x).
func TestEstimateChineseWeighting(t *testing.T) {
	cn := &Message{Role: RoleUser, Content: []Block{TextBlock("中")}} // 100 CJK
	for i := 0; i < 99; i++ {
		cn.Content[0].Text += "中"
	}
	en := &Message{Role: RoleUser, Content: []Block{TextBlock(strings.Repeat("a", 100))}}
	cnTok := EstimateMessageTokens(cn)
	enTok := EstimateMessageTokens(en)
	if cnTok != 100 {
		t.Errorf("100 CJK tokens = %d, want 100", cnTok)
	}
	if enTok != 25 {
		t.Errorf("100 ASCII tokens = %d, want 25", enTok)
	}
	if cnTok <= enTok {
		t.Errorf("Chinese (%d) should estimate higher than ASCII (%d)", cnTok, enTok)
	}
}

// The newest assistant with real usage anchors the total; trailing tool
// results are character-estimated and added on.
func TestEstimateTokensAnchor(t *testing.T) {
	user := NewUserMessage("hello there") // 11 ascii = 3
	asst := &Message{Role: RoleAssistant, Content: []Block{TextBlock("hi")}, Usage: &Usage{InputTokens: 400, OutputTokens: 100, TotalTokens: 500}}
	tool := NewToolResultMessage(ToolCall{ID: "tc1", Name: "now"}, ToolResult{Output: strings.Repeat("x", 200)})
	got := EstimateTokens([]*Message{user, asst, tool})
	want := 500 + EstimateMessageTokens(tool) // anchor + trailing tool estimate (text + id/name)
	if got != want {
		t.Errorf("EstimateTokens with anchor = %d, want %d", got, want)
	}
}

// No real usage anywhere → sum of character estimates.
func TestEstimateTokensNoUsage(t *testing.T) {
	msgs := []*Message{
		NewUserMessage(strings.Repeat("a", 40)), // 10
		NewUserMessage(strings.Repeat("b", 80)), // 20
	}
	if got := EstimateTokens(msgs); got != 30 {
		t.Errorf("EstimateTokens no-usage = %d, want 30", got)
	}
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("EstimateTokens(nil) = %d, want 0", got)
	}
}

// Only the newest assistant's usage counts as the anchor; an older one with
// usage is irrelevant.
func TestEstimateTokensNewestUsageWins(t *testing.T) {
	old := &Message{Role: RoleAssistant, Content: []Block{TextBlock("a")}, Usage: &Usage{TotalTokens: 10}}
	tool := NewToolResultMessage(ToolCall{ID: "c", Name: "n"}, ToolResult{Output: strings.Repeat("y", 40)}) // 10
	newest := &Message{Role: RoleAssistant, Content: []Block{TextBlock("b")}, Usage: &Usage{TotalTokens: 1000}}
	got := EstimateTokens([]*Message{old, tool, newest})
	// anchor = newest (1000); nothing after it → 1000.
	if got != 1000 {
		t.Errorf("EstimateTokens newest-wins = %d, want 1000", got)
	}
}

// Tool-call arguments are counted toward the estimate.
func TestEstimateTokensToolCallArgs(t *testing.T) {
	// 400-arg JSON ≈ 100 tokens (ASCII/4) + name/id overhead, clearly > 0
	// and larger than the same call with empty args.
	empty := NewToolResultMessage(ToolCall{ID: "c", Name: "n"}, ToolResult{})
	big := &Message{Role: RoleAssistant, Content: []Block{
		ToolCallBlock("c", "tool", json.RawMessage(`{"k":"`+strings.Repeat("v", 400)+`"}`)),
	}}
	if got := EstimateMessageTokens(big); got < 100 {
		t.Errorf("tool-call args estimate = %d, want >= 100", got)
	}
	if EstimateMessageTokens(big) <= EstimateMessageTokens(empty) {
		t.Errorf("args should add tokens: big=%d empty=%d", EstimateMessageTokens(big), EstimateMessageTokens(empty))
	}
}
