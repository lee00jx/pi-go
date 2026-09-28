package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/lee00jx/pi-go/core"
)

// fsPath resolves a (possibly relative) tool path against rootDir. No sandbox
// is applied: `..` and symlinks are not checked (1:1 with pi). An empty rootDir
// falls back to the process working directory.
func fsPath(rootDir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	base := rootDir
	if base == "" {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}
	return filepath.Join(base, p)
}

// ---- read ----

// ReadTool reads a text file, head-truncated and pageable via offset/limit.
type ReadTool struct {
	RootDir string
	Logger  *slog.Logger
}

var (
	_ core.Tool          = ReadTool{}
	_ core.DescribedTool = ReadTool{}
)

func NewRead(rootDir string) ReadTool { return ReadTool{RootDir: rootDir} }

func (ReadTool) Name() string { return "read" }

func (ReadTool) Description() string {
	return "Read a text file. Use offset (1-based line) and limit for large files. Binary files are rejected. Output is head-truncated to 2000 lines or 50KB."
}

func (ReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"1-based first line to return"},"limit":{"type":"integer","description":"max lines to return"}},"required":["path"]}`)
}

func (ReadTool) ExecutionMode() core.Mode { return core.ModeParallel }

func (t ReadTool) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" {
		return core.ToolResult{Output: "read: missing required 'path'", IsError: true}, nil
	}
	full := fsPath(t.RootDir, args.Path)
	data, err := os.ReadFile(full)
	if err != nil {
		return core.ToolResult{Output: fmt.Sprintf("read: %v", err), IsError: true}, nil
	}
	if hasNUL(data[:min(len(data), 8192)]) {
		return core.ToolResult{Output: fmt.Sprintf("%s looks like a binary file; not readable as text", full)}, nil
	}
	text := string(data)
	if fl := lenFirstLine(text); fl > defaultMaxBytes {
		return core.ToolResult{
			Output: fmt.Sprintf("first line of %s is %d bytes (over %d); narrow it with offset/limit, e.g. `sed -n '<n>p' | head -c 51200`", full, fl, defaultMaxBytes),
		}, nil
	}
	lines := strings.Split(text, "\n")
	if args.Offset > 0 {
		start := args.Offset - 1
		if start >= len(lines) {
			return core.ToolResult{Output: fmt.Sprintf("offset %d is beyond end of %s (%d lines)", args.Offset, full, len(lines)), IsError: true}, nil
		}
		lines = lines[start:]
	}
	if args.Limit > 0 && len(lines) > args.Limit {
		lines = lines[:args.Limit]
	}
	out := strings.Join(lines, "\n")
	out, trunc := truncateHead(out, defaultMaxLines, defaultMaxBytes)
	if trunc {
		out += fmt.Sprintf("\n[Showing lines 1-%d of %d total. Use offset to continue.]",
			len(strings.Split(out, "\n"))-1, len(strings.Split(text, "\n")))
	}
	if t.Logger != nil {
		t.Logger.Info("read", "path", full, "bytes", len(out), "truncated", trunc)
	}
	return core.ToolResult{Output: out}, nil
}

// ---- write ----

// WriteTool writes a file, creating parent directories as needed. No
// confirmation gate (1:1 with pi).
type WriteTool struct {
	RootDir string
	Logger  *slog.Logger
}

var (
	_ core.Tool          = WriteTool{}
	_ core.DescribedTool = WriteTool{}
)

func NewWrite(rootDir string) WriteTool { return WriteTool{RootDir: rootDir} }

func (WriteTool) Name() string { return "write" }

func (WriteTool) Description() string {
	return "Write content to a file, creating parent directories as needed. Overwrites an existing file."
}

func (WriteTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)
}

func (WriteTool) ExecutionMode() core.Mode { return core.ModeSequential }

func (t WriteTool) Execute(ctx context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	if ctx.Err() != nil {
		return core.ToolResult{Output: "Operation aborted", IsError: true}, nil
	}
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" {
		return core.ToolResult{Output: "write: missing required 'path'/'content'", IsError: true}, nil
	}
	full := fsPath(t.RootDir, args.Path)
	if dir := filepath.Dir(full); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return core.ToolResult{Output: fmt.Sprintf("write: mkdir: %v", err), IsError: true}, nil
		}
	}
	if err := os.WriteFile(full, []byte(args.Content), 0o644); err != nil {
		return core.ToolResult{Output: fmt.Sprintf("write: %v", err), IsError: true}, nil
	}
	if t.Logger != nil {
		t.Logger.Info("write", "path", full, "bytes", len(args.Content))
	}
	return core.ToolResult{Output: fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), full)}, nil
}

// ---- edit ----

// EditTool applies exact string replacements to a file. All oldText values are
// matched against the ORIGINAL content; each must be unique and non-overlapping.
// BOM and CRLF are detected and preserved on write.
type EditTool struct {
	RootDir string
	Logger  *slog.Logger
}

var (
	_ core.Tool          = EditTool{}
	_ core.DescribedTool = EditTool{}
)

func NewEdit(rootDir string) EditTool { return EditTool{RootDir: rootDir} }

func (EditTool) Name() string { return "edit" }

func (EditTool) Description() string {
	return "Apply exact string replacements to a file. Each oldText is matched against the original content and must be unique and non-overlapping."
}

func (EditTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["oldText","newText"]},"description":"exact replacements matched against the ORIGINAL content; each oldText must be unique and non-overlapping"}},"required":["path","edits"]}`)
}

func (EditTool) ExecutionMode() core.Mode { return core.ModeSequential }

type editOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// editSpan pairs a matched region with its replacement text so the pair stays
// aligned through sorting and back-to-front application.
type editSpan struct {
	start, end int
	new        string
}

func (t EditTool) Execute(ctx context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	if ctx.Err() != nil {
		return core.ToolResult{Output: "Operation aborted", IsError: true}, nil
	}
	var args struct {
		Path    string   `json:"path"`
		Edits   []editOp `json:"edits"`
		OldText string   `json:"oldText"`
		NewText string   `json:"newText"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" {
		return core.ToolResult{Output: "edit: missing required 'path'/'edits'", IsError: true}, nil
	}
	if len(args.Edits) == 0 && args.OldText != "" {
		args.Edits = []editOp{{OldText: args.OldText, NewText: args.NewText}}
	}
	if len(args.Edits) == 0 {
		return core.ToolResult{Output: "edit: no edits provided", IsError: true}, nil
	}

	full := fsPath(t.RootDir, args.Path)
	data, err := os.ReadFile(full)
	if err != nil {
		return core.ToolResult{Output: fmt.Sprintf("edit: %v", err), IsError: true}, nil
	}
	// Detect BOM and CRLF, normalize to \n for matching, restore on write.
	bom := ""
	body := string(data)
	if strings.HasPrefix(body, "\ufeff") {
		bom = "\ufeff"
		body = body[len(bom):]
	}
	crlf := strings.Contains(body, "\r\n")
	work := strings.ReplaceAll(body, "\r\n", "\n")

	spans := make([]editSpan, 0, len(args.Edits))
	for i, e := range args.Edits {
		if e.OldText == "" {
			return core.ToolResult{Output: fmt.Sprintf("edit: edit #%d has empty oldText", i+1), IsError: true}, nil
		}
		switch strings.Count(work, e.OldText) {
		case 0:
			return core.ToolResult{Output: fmt.Sprintf("edit: oldText of edit #%d not found in %s", i+1, full), IsError: true}, nil
		case 1:
		default:
			return core.ToolResult{Output: fmt.Sprintf("edit: oldText of edit #%d is ambiguous (found multiple times) in %s; make it unique", i+1, full), IsError: true}, nil
		}
		idx := strings.Index(work, e.OldText)
		spans = append(spans, editSpan{idx, idx + len(e.OldText), e.NewText})
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].start < spans[j].end && spans[j].start < spans[i].end {
				return core.ToolResult{Output: fmt.Sprintf("edit: edits #%d and #%d overlap", i+1, j+1), IsError: true}, nil
			}
		}
	}
	if allNoChange(work, spans) {
		return core.ToolResult{Output: "edit: no changes (oldText and newText are identical)", IsError: true}, nil
	}
	// Apply from the end so earlier offsets stay valid. Spans (and their
	// replacement text) are sorted together, then applied back-to-front.
	sortSpans(spans)
	out := work
	for k := len(spans) - 1; k >= 0; k-- {
		out = out[:spans[k].start] + spans[k].new + out[spans[k].end:]
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := os.WriteFile(full, []byte(bom+out), 0o644); err != nil {
		return core.ToolResult{Output: fmt.Sprintf("edit: write: %v", err), IsError: true}, nil
	}
	if t.Logger != nil {
		t.Logger.Info("edit", "path", full, "edits", len(spans))
	}
	return core.ToolResult{Output: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(spans), full)}, nil
}

func allNoChange(work string, spans []editSpan) bool {
	for _, sp := range spans {
		if work[sp.start:sp.end] != sp.new {
			return false
		}
	}
	return true
}

func sortSpans(spans []editSpan) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

func hasNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

func lenFirstLine(s string) int {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return i
	}
	return len(s)
}
