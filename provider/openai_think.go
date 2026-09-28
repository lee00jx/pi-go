package provider

import "strings"

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// thinkSplit splits Qwen/vLLM content that inlines thinking between
// <think>…</think> from visible assistant text. Incomplete tags are held
// back across Feed calls so a tag split over SSE chunks is not leaked.
type thinkSplit struct {
	inThink bool
	hold    string
}

func (s *thinkSplit) Feed(delta string) (thinking, text string) {
	if delta == "" {
		return "", ""
	}
	s.hold += delta
	var th, tx strings.Builder
	for {
		if s.inThink {
			i := strings.Index(s.hold, thinkClose)
			if i < 0 {
				safe := holdBack(s.hold, thinkClose)
				if safe > 0 {
					th.WriteString(s.hold[:safe])
					s.hold = s.hold[safe:]
				}
				return th.String(), tx.String()
			}
			th.WriteString(s.hold[:i])
			s.hold = s.hold[i+len(thinkClose):]
			s.inThink = false
			continue
		}
		i := strings.Index(s.hold, thinkOpen)
		if i < 0 {
			safe := holdBack(s.hold, thinkOpen)
			if safe > 0 {
				tx.WriteString(s.hold[:safe])
				s.hold = s.hold[safe:]
			}
			return th.String(), tx.String()
		}
		tx.WriteString(s.hold[:i])
		s.hold = s.hold[i+len(thinkOpen):]
		s.inThink = true
	}
}

func (s *thinkSplit) Finish() (thinking, text string) {
	rest := s.hold
	s.hold = ""
	if rest == "" {
		return "", ""
	}
	if s.inThink {
		return rest, ""
	}
	return "", rest
}

func holdBack(buf, tag string) int {
	n := len(buf)
	max := len(tag) - 1
	if max > n {
		max = n
	}
	for k := max; k >= 1; k-- {
		if strings.HasSuffix(buf, tag[:k]) {
			return n - k
		}
	}
	return n
}
