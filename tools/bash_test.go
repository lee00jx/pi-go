package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
)

func mustCall(t *testing.T, name string, args map[string]any) core.ToolCall {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return core.ToolCall{Name: name, Arguments: b}
}

func runTool(t *testing.T, tool core.Tool, name string, args map[string]any) core.ToolResult {
	t.Helper()
	res, err := tool.Execute(context.Background(), mustCall(t, name, args), nil)
	if err != nil {
		t.Fatalf("%s: Execute returned error: %v", name, err)
	}
	return res
}

func runBash(t *testing.T, command string, ctx context.Context) core.ToolResult {
	t.Helper()
	res, err := Bash{}.Execute(ctx, mustCall(t, "bash", map[string]any{"command": command}), nil)
	if err != nil {
		t.Fatalf("bash: Execute returned error: %v", err)
	}
	return res
}

func TestBashEcho(t *testing.T) {
	if d := core.ToolDescription(Bash{}); d == "" {
		t.Fatal("bash should implement DescribedTool")
	}
	res := runBash(t, "echo hello-pi-go", context.Background())
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello-pi-go") {
		t.Fatalf("output = %q, want to contain hello-pi-go", res.Output)
	}
}

func TestBashExitCode(t *testing.T) {
	res := runBash(t, "exit 3", context.Background())
	if !res.IsError {
		t.Fatalf("expected IsError for exit 3, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "[exit code 3]") {
		t.Fatalf("output = %q, want [exit code 3]", res.Output)
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := runBash(t, "sleep 10", ctx)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Fatalf("expected error on timeout, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "terminated") {
		t.Fatalf("output = %q, want terminated marker", res.Output)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s, want well under 5s (process group should be killed)", elapsed)
	}
}

func TestBashTruncation(t *testing.T) {
	res := runBash(t, "seq 1 5000", context.Background())
	if !strings.Contains(res.Output, "output truncated") {
		t.Fatalf("output (%d bytes) missing truncation notice", len(res.Output))
	}
	if !strings.Contains(res.Output, "5000") {
		t.Fatalf("tail truncation should keep the last lines (5000): %q", tailBytes(res.Output, 80))
	}
}

func TestTruncateTailUTF8(t *testing.T) {
	s := strings.Repeat("汉", 20000) // 60000 bytes, one repeated rune
	out, trunc := truncateTail(s, defaultMaxLines, defaultMaxBytes)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if len(out) > defaultMaxBytes {
		t.Fatalf("truncated len %d > max %d", len(out), defaultMaxBytes)
	}
	for _, r := range out {
		if r != '汉' {
			t.Fatalf("unexpected rune %q (rune split?) in output", r)
		}
	}
}

func TestTruncateHead(t *testing.T) {
	s := strings.Repeat("line\n", 3000) // 15000 bytes, 3000 lines
	out, trunc := truncateHead(s, defaultMaxLines, defaultMaxBytes)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if !strings.HasPrefix(out, "line\n") {
		t.Fatalf("head truncation should keep the first line, got %q...", out[:min(40, len(out))])
	}
}

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
