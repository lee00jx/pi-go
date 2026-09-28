package tools

import (
	"strings"
	"unicode/utf8"
)

// Output capture limits shared by the built-in tools (mirrors pi: 2000 lines
// or 50KB, whichever comes first).
const (
	defaultMaxLines = 2000
	defaultMaxBytes = 50 * 1024
)

// countLines is a cheap line count (a trailing newline does not add a line).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// splitLines splits on "\n" without keeping the separators.
func splitLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// truncateTail keeps the TAIL of s, bounded by maxLines and maxBytes (whichever
// is tighter). It never splits a UTF-8 rune and never returns a partial line at
// the start. bash uses it because it cares about the most recent output.
func truncateTail(s string, maxLines, maxBytes int) (string, bool) {
	if len(s) <= maxBytes && countLines(s) <= maxLines {
		return s, false
	}
	out := s
	// Byte bound first (UTF-8 safe), then drop a leading partial line.
	if len(out) > maxBytes {
		start := len(out) - maxBytes
		for start < len(out) && !utf8.RuneStart(out[start]) {
			start++
		}
		if idx := strings.IndexByte(out[start:], '\n'); idx >= 0 {
			start += idx + 1
		}
		out = out[start:]
	}
	// Line bound: keep the last maxLines lines.
	if lines := splitLines(out); len(lines) > maxLines {
		out = strings.Join(lines[len(lines)-maxLines:], "\n")
		if strings.HasSuffix(s, "\n") {
			out += "\n"
		}
	}
	return out, true
}

// truncateHead keeps the HEAD of s, bounded by maxLines and maxBytes (whichever
// is tighter). It never splits a UTF-8 rune and never returns a partial line at
// the end. read uses it because it pages forward. If the first line alone
// exceeds maxBytes it returns an empty string (the caller surfaces a hint).
func truncateHead(s string, maxLines, maxBytes int) (string, bool) {
	if len(s) <= maxBytes && countLines(s) <= maxLines {
		return s, false
	}
	out := s
	// Byte bound first (UTF-8 safe), then drop back to the last full line.
	if len(out) > maxBytes {
		end := maxBytes
		for end > 0 && !utf8.RuneStart(out[end]) {
			end--
		}
		cut := out[:end]
		if idx := strings.LastIndexByte(cut, '\n'); idx >= 0 {
			out = cut[:idx+1]
		} else {
			out = "" // the first line alone exceeds the limit
		}
	}
	// Line bound: keep the first maxLines lines.
	if lines := splitLines(out); len(lines) > maxLines {
		out = strings.Join(lines[:maxLines], "\n")
		if strings.HasSuffix(s, "\n") {
			out += "\n"
		}
	}
	return out, true
}
