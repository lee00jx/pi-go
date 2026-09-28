package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file is a self-written port of pi's
// packages/ai/src/utils/json-parse.ts. Go has no equivalent of the npm
// partial-json library, so the "close whatever is open" step (closePartial)
// is implemented here; the escape-repair logic (RepairJSON) is a 1:1
// translation.

// ParseSalvage attempts to parse JSON that may be truncated mid-stream.
// It degrades in four steps and ALWAYS returns a value:
//
//  1. plain json.Unmarshal
//  2. Unmarshal after RepairJSON (escape repair)
//  3. Unmarshal after closePartial (auto-close strings/objects/arrays)
//  4. empty object
//
// ok reports whether the input parsed as-is (steps 1-2); a
// truncated-but-salvaged object (step 3) reports ok=false so callers can
// apply truncation protection (DESIGN §2.3).
func ParseSalvage(raw string) (v map[string]any, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, false
	}
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v, true
	}
	if repaired := RepairJSON(raw); repaired != raw {
		if err := json.Unmarshal([]byte(repaired), &v); err == nil {
			return v, false
		}
	}
	for _, candidate := range []string{closePartial(raw), closePartial(RepairJSON(raw))} {
		var out map[string]any
		if err := json.Unmarshal([]byte(candidate), &out); err == nil {
			return out, false
		}
	}
	return map[string]any{}, false
}

var validJSONEscapes = map[byte]bool{
	'"': true, '\\': true, '/': true,
	'b': true, 'f': true, 'n': true, 'r': true, 't': true, 'u': true,
}

func isControlChar(c byte) bool { return c >= 0x00 && c <= 0x1f }

func escapeControlChar(c byte) string {
	switch c {
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	default:
		return fmt.Sprintf("\\u%04x", c)
	}
}

// RepairJSON fixes malformed JSON string literals by escaping raw control
// characters and doubling backslashes that precede invalid escapes. It is a
// direct translation of pi's repairJson.
func RepairJSON(in string) string {
	var b strings.Builder
	b.Grow(len(in) + 16)
	inString := false
	for i := 0; i < len(in); {
		c := in[i]
		if !inString {
			b.WriteByte(c)
			if c == '"' {
				inString = true
			}
			i++
			continue
		}
		switch {
		case c == '"':
			b.WriteByte(c)
			inString = false
			i++
		case c == '\\':
			if i+1 >= len(in) {
				b.WriteString(`\\`) // trailing backslash: make it a literal
				i++
				continue
			}
			next := in[i+1]
			if next == 'u' {
				if i+6 <= len(in) && isHex(in[i+2:i+6]) {
					b.WriteString(`\u`)
					b.WriteString(in[i+2 : i+6])
					i += 6
					continue
				}
				b.WriteString(`\\`)
				i++
				continue
			}
			if validJSONEscapes[next] {
				b.WriteByte('\\')
				b.WriteByte(next)
				i += 2
				continue
			}
			b.WriteString(`\\`)
			i++
		default:
			// Only ASCII control chars are illegal raw in JSON strings;
			// multibyte UTF-8 bytes are all >= 0x80 and pass through.
			if isControlChar(c) {
				b.WriteString(escapeControlChar(c))
			} else {
				b.WriteByte(c)
			}
			i++
		}
	}
	return b.String()
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) == 4
}

// closePartial best-effort completes a truncated JSON value so it parses:
// it cuts at the last complete value (or at the start of an unterminated
// string), drops dangling keys/colons/commas, and closes open objects and
// arrays in reverse order.
func closePartial(in string) string {
	// Object frames track how far the key:value pair has progressed.
	const (
		wantKey    byte = iota // after '{' or ','
		wantColon              // key present, ':' missing
		wantValue              // ':' present, value missing
		afterValue             // waiting for ',' or '}'
	)
	type frame struct {
		isArray bool
		stage   byte
	}

	var stack []frame
	inString := false
	escaped := false
	stringStart := 0
	lastValueEnd := 0 // index just past the last complete value

	for i := 0; i < len(in); {
		c := in[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
				if len(stack) > 0 && !stack[len(stack)-1].isArray && stack[len(stack)-1].stage == wantKey {
					stack[len(stack)-1].stage = wantColon
				} else {
					lastValueEnd = i + 1
					if len(stack) > 0 && !stack[len(stack)-1].isArray {
						stack[len(stack)-1].stage = afterValue
					}
				}
			}
			i++
			continue
		}
		switch c {
		case '"':
			inString = true
			stringStart = i
			i++
		case '{':
			stack = append(stack, frame{stage: wantKey})
			i++
		case '[':
			stack = append(stack, frame{isArray: true})
			i++
		case '}':
			if len(stack) > 0 && !stack[len(stack)-1].isArray {
				stack = stack[:len(stack)-1]
			}
			lastValueEnd = i + 1
			i++
		case ']':
			if len(stack) > 0 && stack[len(stack)-1].isArray {
				stack = stack[:len(stack)-1]
			}
			lastValueEnd = i + 1
			i++
		case ',':
			if len(stack) > 0 && !stack[len(stack)-1].isArray {
				stack[len(stack)-1].stage = wantKey
			}
			i++
		case ':':
			if len(stack) > 0 && !stack[len(stack)-1].isArray && stack[len(stack)-1].stage == wantColon {
				stack[len(stack)-1].stage = wantValue
			}
			i++
		default:
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				i++
				continue
			}
			// Number / literal (true/false/null) / bare key token.
			j := i
			for j < len(in) {
				k := in[j]
				if k == ' ' || k == '\t' || k == '\n' || k == '\r' ||
					k == ',' || k == ':' || k == '}' || k == ']' || k == '{' || k == '[' || k == '"' {
					break
				}
				j++
			}
			lastValueEnd = j
			if len(stack) > 0 && !stack[len(stack)-1].isArray && stack[len(stack)-1].stage == wantValue {
				stack[len(stack)-1].stage = afterValue
			}
			i = j
		}
	}

	cut := lastValueEnd
	if inString {
		// Unterminated string: it is the trailing partial token.
		cut = stringStart
	}
	out := in[:cut]

	// Drop dangling punctuation/keys so the cut string can be closed.
	for {
		out = strings.TrimRight(out, " \t\n\r")
		dropped := false
		if strings.HasSuffix(out, ",") {
			out = out[:len(out)-1]
			dropped = true
		} else if len(stack) > 0 && !stack[len(stack)-1].isArray {
			top := &stack[len(stack)-1]
			if strings.HasSuffix(out, ":") {
				out = strings.TrimRight(out[:len(out)-1], " \t\n\r")
				out = dropToken(out)
				top.stage = wantKey
				dropped = true
			} else if top.stage == wantColon {
				out = dropToken(out)
				top.stage = wantKey
				dropped = true
			}
		}
		if !dropped {
			break
		}
	}

	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].isArray {
			out += "]"
		} else {
			out += "}"
		}
	}
	return out
}

// dropToken removes the last key token (quoted string or bare word) from out.
func dropToken(out string) string {
	if out == "" {
		return out
	}
	if out[len(out)-1] == '"' {
		// Find the opening quote of the key string (best effort: escaped
		// quotes in tool-argument keys do not occur in practice).
		for i := len(out) - 2; i >= 0; i-- {
			if out[i] == '\\' {
				i--
			} else if out[i] == '"' {
				return out[:i]
			}
		}
		return ""
	}
	j := len(out)
	for j > 0 {
		c := out[j-1]
		if c == '"' || c == '{' || c == '}' || c == '[' || c == ']' ||
			c == ',' || c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		j--
	}
	return out[:j]
}
