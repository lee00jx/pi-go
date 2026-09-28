package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/lee00jx/pi-go/core"
)

func TestThinkSplitAcrossChunks(t *testing.T) {
	var s thinkSplit
	th, tx := s.Feed("<th")
	if th != "" || tx != "" {
		t.Fatalf("partial open tag leaked: thinking=%q text=%q", th, tx)
	}
	th, tx = s.Feed("ink>\nplan")
	if th != "\nplan" || tx != "" {
		t.Fatalf("after open: thinking=%q text=%q", th, tx)
	}
	th, tx = s.Feed("</thi")
	if th != "" || tx != "" {
		t.Fatalf("partial close leaked: thinking=%q text=%q", th, tx)
	}
	th, tx = s.Feed("nk>\nhello")
	if th != "" || tx != "\nhello" {
		t.Fatalf("after close: thinking=%q text=%q", th, tx)
	}
	th, tx = s.Finish()
	if th != "" || tx != "" {
		t.Fatalf("finish leftover: thinking=%q text=%q", th, tx)
	}
}

func TestThinkSplitPlainText(t *testing.T) {
	var s thinkSplit
	th, tx := s.Feed("hello < world")
	if th != "" || tx != "hello < world" {
		t.Fatalf("plain: thinking=%q text=%q", th, tx)
	}
}

func TestOpenAIReasoningContentStream(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"step "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"one"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, nil)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	var thinking, text string
	var done *core.StreamEvent
	for ev := range ch {
		switch ev.Type {
		case core.StreamThinkingDelta:
			thinking += ev.Text
		case core.StreamTextDelta:
			text += ev.Text
		case core.StreamDone:
			done = &ev
		}
	}
	if thinking != "step one" || text != "answer" {
		t.Fatalf("thinking=%q text=%q", thinking, text)
	}
	if done == nil || done.Message == nil {
		t.Fatal("missing done message")
	}
	blocks := done.Message.Content
	if len(blocks) != 2 || blocks[0].Type != core.BlockThinking || blocks[0].Thinking != "step one" {
		t.Fatalf("done blocks = %+v", blocks)
	}
	if blocks[1].Type != core.BlockText || blocks[1].Text != "answer" {
		t.Fatalf("done text block = %+v", blocks[1])
	}
}

func TestOpenAIThinkTagStream(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"<think>"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"why"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"</think>"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}, nil)
	defer srv.Close()

	client := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	ch, err := client.Stream(context.Background(), req(t))
	if err != nil {
		t.Fatal(err)
	}
	var thinking, text string
	for ev := range ch {
		switch ev.Type {
		case core.StreamThinkingDelta:
			thinking += ev.Text
		case core.StreamTextDelta:
			text += ev.Text
		}
	}
	if strings.Contains(text, "<think>") || strings.Contains(text, "why") {
		t.Fatalf("think tags leaked into text=%q", text)
	}
	if thinking != "why" || text != "ok" {
		t.Fatalf("thinking=%q text=%q", thinking, text)
	}
}
