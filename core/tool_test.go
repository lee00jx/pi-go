package core

import (
	"context"
	"encoding/json"
	"testing"
)

type plainDescTool struct{}

func (plainDescTool) Name() string { return "plain" }
func (plainDescTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (plainDescTool) ExecutionMode() Mode { return ModeParallel }
func (plainDescTool) Execute(context.Context, ToolCall, func(ToolUpdate)) (ToolResult, error) {
	return ToolResult{}, nil
}

type describedDescTool struct{ plainDescTool }

func (describedDescTool) Description() string { return "  says what this tool does  " }

func TestToolDescription(t *testing.T) {
	if got := ToolDescription(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
	if got := ToolDescription(plainDescTool{}); got != "" {
		t.Fatalf("plain: %q", got)
	}
	if got := ToolDescription(describedDescTool{}); got != "says what this tool does" {
		t.Fatalf("described: %q", got)
	}
}
