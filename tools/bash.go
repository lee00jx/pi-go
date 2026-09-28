package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lee00jx/pi-go/core"
)

// Bash runs a shell command and returns its combined stdout/stderr. It mirrors
// pi's bash tool: plain pipes (no PTY), tail-truncated output (2000 lines /
// 50KB), and a process-group SIGKILL on timeout or cancel. There is
// deliberately NO command allow/deny list and NO path sandbox (1:1 with pi);
// an integrator gates dangerous commands through a BeforeToolCall hook.
//
// POSIX only (process-group kill + WaitStatus); Windows needs build-tag
// variants before it can be used there.
type Bash struct {
	// Cwd is the working directory for commands; empty uses the process cwd.
	Cwd string
	// Logger for structured per-run logs; nil → slog.Default().
	Logger *slog.Logger
}

var (
	_ core.Tool          = Bash{}
	_ core.DescribedTool = Bash{}
)

func NewBash(cwd string) Bash { return Bash{Cwd: cwd} }

func (Bash) Name() string { return "bash" }

func (Bash) Description() string {
	return "Execute a shell command in the current working directory. Returns combined stdout and stderr. Output is tail-truncated to 2000 lines or 50KB."
}

func (Bash) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the shell command to run"}},"required":["command"]}`)
}

func (Bash) ExecutionMode() core.Mode { return core.ModeSequential }

func (b Bash) Execute(ctx context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
	log := b.Logger
	if log == nil {
		log = slog.Default()
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || strings.TrimSpace(args.Command) == "" {
		return core.ToolResult{Output: "bash: missing required 'command'", IsError: true}, nil
	}

	shell, shellArgs := pickShell(args.Command)
	cmd := exec.Command(shell, shellArgs...)
	if b.Cwd != "" {
		cmd.Dir = b.Cwd
	}
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	// Merge stdout + stderr into one pipe so their output stays interleaved in
	// the order the child actually produced it.
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return core.ToolResult{Output: fmt.Sprintf("bash: pipe: %v", err), IsError: true}, nil
	}
	cmd.Stdout = pipeW
	cmd.Stderr = pipeW
	if err := cmd.Start(); err != nil {
		return core.ToolResult{Output: fmt.Sprintf("bash: start: %v", err), IsError: true}, nil
	}
	// The child now holds its own dup of the write end; drop ours so the read
	// end reaches EOF once the whole process group exits.
	_ = pipeW.Close()

	start := time.Now()
	buf := tailBuffer{max: defaultMaxBytes}
	done := make(chan error, 1)
	go func() {
		// Drain continuously so the child never blocks on a full pipe, then
		// Wait (only safe once the pipe is fully read).
		_, _ = io.Copy(&buf, pipeR)
		_ = pipeR.Close()
		done <- cmd.Wait()
	}()

	var waitErr error
	cancelled := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		cancelled = true
		killProcessGroup(cmd)
		<-done // unblocks once the killed child closes the pipe
		waitErr = ctx.Err()
	}

	out := buf.String()
	out, trunc := truncateTail(out, defaultMaxLines, defaultMaxBytes)
	res := finalizeBash(out, cmd, waitErr, trunc, cancelled)
	log.Info("bash",
		"cmd", summarizeCmd(args.Command),
		"duration_ms", time.Since(start).Milliseconds(),
		"bytes", len(out),
		"is_error", res.IsError,
	)
	return res, nil
}

// pickShell prefers bash, falling back to sh.
func pickShell(command string) (string, []string) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-c", command}
	}
	return "sh", []string{"-c", command}
}

// killProcessGroup kills the child and everything it spawned. Because the child
// is its own process-group leader (Setpgid), a negative PID targets the whole
// tree — killing only the shell would orphan grandchild processes.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill() // ESRCH if it already exited
	}
}

func finalizeBash(out string, cmd *exec.Cmd, waitErr error, trunc, cancelled bool) core.ToolResult {
	if out == "" {
		out = "(no output)"
	}
	if trunc {
		out += fmt.Sprintf("\n[output truncated: showing last %d lines / 50KB]", defaultMaxLines)
	}
	if cancelled {
		return core.ToolResult{Output: out + "\n[terminated: timeout or run cancelled]", IsError: true}
	}
	if waitErr == nil {
		return core.ToolResult{Output: out}
	}
	if code := exitCode(cmd, waitErr); code >= 0 {
		return core.ToolResult{Output: out + fmt.Sprintf("\n[exit code %d]", code), IsError: code != 0}
	}
	return core.ToolResult{Output: out + fmt.Sprintf("\n[terminated: %v]", waitErr), IsError: true}
}

// exitCode maps a Wait error to an exit status, using 128+signal for
// signal-killed children (pi convention).
func exitCode(cmd *exec.Cmd, waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		if wsc, ok := ee.Sys().(syscall.WaitStatus); ok && wsc.Signaled() {
			return 128 + int(wsc.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

func summarizeCmd(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// tailBuffer keeps only the last max bytes written — enough for truncateTail
// after the command finishes, without unbounded memory on huge outputs. It is
// not line/rune aware on its own; truncateTail finalizes the boundary.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
