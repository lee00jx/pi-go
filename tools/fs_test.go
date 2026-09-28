package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHeadTruncation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, NewRead(dir), "read", map[string]any{"path": "big.txt"})
	if !strings.Contains(res.Output, "Use offset to continue") {
		t.Fatalf("want head-truncation notice, got %q...", headBytes(res.Output, 100))
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\ne"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, NewRead(dir), "read", map[string]any{"path": "f.txt", "offset": 2, "limit": 2})
	if strings.TrimSpace(res.Output) != "b\nc" {
		t.Fatalf("offset=2 limit=2 → %q, want %q", res.Output, "b\nc")
	}
}

func TestReadBinary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bin")
	if err := os.WriteFile(p, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, NewRead(dir), "read", map[string]any{"path": "bin"})
	if !strings.Contains(res.Output, "binary") {
		t.Fatalf("want binary notice, got %q", res.Output)
	}
}

func TestWriteCreatesParents(t *testing.T) {
	dir := t.TempDir()
	res := runTool(t, NewWrite(dir), "write", map[string]any{"path": "a/b/c.txt", "content": "hi"})
	if res.IsError {
		t.Fatalf("write error: %s", res.Output)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a/b/c.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("readback = %q, err %v", got, err)
	}
}

func TestEditMultiple(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "e.txt")
	if err := os.WriteFile(p, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, NewEdit(dir), "edit", map[string]any{
		"path":  "e.txt",
		"edits": []map[string]string{{"oldText": "beta", "newText": "BETA"}, {"oldText": "alpha", "newText": "ALPHA"}},
	})
	if res.IsError {
		t.Fatalf("edit error: %s", res.Output)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "ALPHA BETA gamma" {
		t.Fatalf("after edit = %q, want %q", got, "ALPHA BETA gamma")
	}
}

func TestEditAmbiguous(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "e.txt")
	if err := os.WriteFile(p, []byte("x x x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, NewEdit(dir), "edit", map[string]any{
		"path":  "e.txt",
		"edits": []map[string]string{{"oldText": "x", "newText": "y"}},
	})
	if !res.IsError || !strings.Contains(res.Output, "ambiguous") {
		t.Fatalf("want ambiguous error, got %q (isError=%v)", res.Output, res.IsError)
	}
}

func TestEditCRLFPreserved(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "crlf.txt")
	if err := os.WriteFile(p, []byte("line1\r\nline2\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, NewEdit(dir), "edit", map[string]any{
		"path":  "crlf.txt",
		"edits": []map[string]string{{"oldText": "line2", "newText": "LINE2"}},
	})
	if res.IsError {
		t.Fatalf("edit error: %s", res.Output)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "line1\r\nLINE2\r\n" {
		t.Fatalf("CRLF not preserved: %q", got)
	}
}

func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
