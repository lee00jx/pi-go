package core

// Token estimation (DESIGN §6.3). Compaction triggers and the
// context-stats endpoint both need a token count for the working context
// without calling the provider. The estimate is deliberately biased high
// (prefer over-counting and compacting early to under-counting and blowing
// the context window).
//
// The two-part rule mirrors the reference implementation:
//
//  1. If the newest assistant message carries real provider usage, its
//     TotalTokens is used as the anchor — the provider already counted
//     everything up to and including that turn. Only messages appended
//     after it (new tool results, new user messages) are estimated by
//     characters.
//  2. Otherwise (no real usage yet), every message is character-estimated.
//
// Character weighting: ASCII runs at 4 chars/token (≈ English), non-ASCII
// (CJK etc.) at 1 char/token. A uniform chars/4 would badly under-count
// Chinese (roughly 1-2 tokens/char), so non-ASCII is counted 1:1.

// charTokens estimates the token count of one string by its characters,
// ASCII at 4 chars/token (rounded up) and non-ASCII at 1 char/token.
func charTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}

// EstimateMessageTokens estimates the token count of a single message by
// the text of all its content blocks (plus tool-role identity fields).
func EstimateMessageTokens(m *Message) int {
	if m == nil {
		return 0
	}
	total := 0
	for _, blk := range m.Content {
		switch blk.Type {
		case BlockText:
			total += charTokens(blk.Text)
		case BlockThinking:
			total += charTokens(blk.Thinking)
		case BlockToolCall:
			total += charTokens(blk.Name) + charTokens(blk.ToolCallID) + charTokens(string(blk.Arguments))
		}
	}
	if m.Role == RoleTool {
		total += charTokens(m.ToolCallID) + charTokens(m.ToolName)
	}
	return total
}

// EstimateTokens estimates the total token count of a working context. See
// the package-level rule above: the newest assistant message with real
// usage anchors the count; trailing messages are character-estimated.
func EstimateTokens(msgs []*Message) int {
	anchor := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant && msgs[i].Usage != nil && msgs[i].Usage.TotalTokens > 0 {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		total := 0
		for _, m := range msgs {
			total += EstimateMessageTokens(m)
		}
		return total
	}
	total := msgs[anchor].Usage.TotalTokens
	for _, m := range msgs[anchor+1:] {
		total += EstimateMessageTokens(m)
	}
	return total
}
