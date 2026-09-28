package gorm_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	gormstore "github.com/lee00jx/pi-go/adapter/gorm"
	"github.com/lee00jx/pi-go/session"
)

// cfg returns a silent GORM config for tests. TranslateError lets the sqlite
// driver map constraint violations to gorm.ErrDuplicatedKey (defense in depth
// on top of the adapter's own existence pre-checks).
func cfg() *gorm.Config {
	return &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	}
}

// openDB opens (and migrates) a GORMStore on a fresh temp SQLite file.
func openDB(t *testing.T) (*gormstore.GORMStore, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000"), cfg())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := gormstore.New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	close := func() {
		if sqlDB, err := db.DB(); err == nil && sqlDB != nil {
			sqlDB.Close()
		}
	}
	return s, close
}

// TestMigrateIdempotent: Migrate can be called more than once (a restarted
// process migrates the same file again).
func TestMigrateIdempotent(t *testing.T) {
	s, closeFn := openDB(t)
	defer closeFn()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestSessionCRUD(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openDB(t)
	defer closeFn()

	meta := session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m", Status: session.StatusIdle}
	if err := s.CreateSession(ctx, meta); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate → ErrSessionExists; empty ID → ErrInvalidMeta.
	if err := s.CreateSession(ctx, meta); !errors.Is(err, session.ErrSessionExists) {
		t.Fatalf("dup create: %v, want ErrSessionExists", err)
	}
	if err := s.CreateSession(ctx, session.SessionMeta{UserID: "u"}); !errors.Is(err, session.ErrInvalidMeta) {
		t.Fatalf("empty id: %v, want ErrInvalidMeta", err)
	}

	got, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "s1" || got.UserID != "u" || got.Model != "m" || got.Status != session.StatusIdle {
		t.Fatalf("get mismatch: %+v", got)
	}
	if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get unknown: %v, want ErrNotFound", err)
	}

	// Update: change status + title (title is a zero→set update, and we must
	// also be able to update back to empty without the zero-value trap).
	got.Status = session.StatusRunning
	got.Title = "work"
	if err := s.UpdateSession(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := s.GetSession(ctx, "s1")
	if got2.Status != session.StatusRunning || got2.Title != "work" {
		t.Fatalf("after update: %+v", got2)
	}
	// Update an unknown session → ErrNotFound.
	if err := s.UpdateSession(ctx, session.SessionMeta{ID: "nope"}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("update unknown: %v, want ErrNotFound", err)
	}

	if err := s.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSession(ctx, "s1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession(ctx, "s1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("double delete: %v, want ErrNotFound", err)
	}
}

func TestListSessions(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openDB(t)
	defer closeFn()

	// Create with small gaps so UpdatedAt is strictly increasing (the store
	// stamps time.Now() internally, so we cannot set it directly).
	mk := func(id, user string) {
		if err := s.CreateSession(ctx, session.SessionMeta{ID: id, UserID: user, Provider: "faux", Model: "m"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mk("a1", "alice")
	mk("a2", "alice")
	mk("b1", "bob")

	all, err := s.ListSessions(ctx, session.SessionQuery{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list all: %d, want 3", len(all))
	}
	// Newest first: b1, a2, a1.
	if all[0].ID != "b1" || all[1].ID != "a2" || all[2].ID != "a1" {
		t.Fatalf("order = %v %v %v, want b1 a2 a1", all[0].ID, all[1].ID, all[2].ID)
	}

	alice, err := s.ListSessions(ctx, session.SessionQuery{UserID: "alice"})
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(alice) != 2 || alice[0].ID != "a2" || alice[1].ID != "a1" {
		t.Fatalf("alice = %+v, want a2 a1", alice)
	}

	limit, err := s.ListSessions(ctx, session.SessionQuery{Limit: 1})
	if err != nil {
		t.Fatalf("list limit: %v", err)
	}
	if len(limit) != 1 || limit[0].ID != "b1" {
		t.Fatalf("limit = %+v, want just b1", limit)
	}
}

func TestListSessionsByAgent(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openDB(t)
	defer closeFn()

	mk := func(id, user, agent string) {
		t.Helper()
		if err := s.CreateSession(ctx, session.SessionMeta{
			ID: id, UserID: user, Agent: agent, Provider: "faux", Model: "m",
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mk("c1", "alice", "calc")
	mk("p1", "alice", "compare")
	mk("c2", "bob", "calc")

	aliceCalc, err := s.ListSessions(ctx, session.SessionQuery{UserID: "alice", Agent: "calc"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(aliceCalc) != 1 || aliceCalc[0].ID != "c1" || aliceCalc[0].Agent != "calc" {
		t.Fatalf("alice calc = %+v, want c1", aliceCalc)
	}

	// UpdateSession must not wipe Agent.
	got, err := s.GetSession(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	got.Status = session.StatusRunning
	got.Agent = "" // caller forgot; GORM must still keep the column
	if err := s.UpdateSession(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetSession(ctx, "c1")
	if got2.Agent != "calc" || got2.Status != session.StatusRunning {
		t.Fatalf("after update: %+v, want agent=calc status=running", got2)
	}
}

func TestEntries(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openDB(t)
	defer closeFn()
	if err := s.CreateSession(ctx, session.SessionMeta{ID: "s1", UserID: "u"}); err != nil {
		t.Fatal(err)
	}

	p1, p2, p3 := []byte(`{"role":"user","text":"hi"}`), []byte(`{"role":"assistant","text":"yo"}`), []byte(`{"toolCallId":"tc1"}`)
	usage := json.RawMessage(`{"input":10,"output":4}`)
	s1, err := s.AppendEntry(ctx, "s1", session.EntryUser, p1, nil)
	if err != nil {
		t.Fatalf("append1: %v", err)
	}
	s2, err := s.AppendEntry(ctx, "s1", session.EntryAssistant, p2, usage)
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	s3, err := s.AppendEntry(ctx, "s1", session.EntryToolResult, p3, nil)
	if err != nil {
		t.Fatalf("append3: %v", err)
	}
	// Seq must be monotonic from 1.
	if s1 != 1 || s2 != 2 || s3 != 3 {
		t.Fatalf("seqs = %d %d %d, want 1 2 3", s1, s2, s3)
	}

	all, err := s.ListEntries(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list all: %d, want 3", len(all))
	}
	if string(all[0].Payload) != string(p1) || all[0].Type != session.EntryUser {
		t.Fatalf("entry[0] = %+v", all[0])
	}
	// Usage round-trips on the assistant entry.
	if string(all[1].Usage) != string(usage) {
		t.Fatalf("entry[1].usage = %s, want %s", all[1].Usage, usage)
	}

	// afterSeq: only seq > 2 → the third entry.
	tail, err := s.ListEntries(ctx, "s1", 2, 0)
	if err != nil {
		t.Fatalf("list tail: %v", err)
	}
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("tail = %+v, want just seq 3", tail)
	}

	// limit: first two.
	first2, err := s.ListEntries(ctx, "s1", 0, 2)
	if err != nil {
		t.Fatalf("list limit: %v", err)
	}
	if len(first2) != 2 || first2[0].Seq != 1 || first2[1].Seq != 2 {
		t.Fatalf("first2 = %+v, want seq 1,2", first2)
	}

	// Empty session: no error, empty list (not ErrNotFound).
	if err := s.CreateSession(ctx, session.SessionMeta{ID: "s2", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	empty, err := s.ListEntries(ctx, "s2", 0, 0)
	if err != nil {
		t.Fatalf("empty list: %v, want nil", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty list: %d entries, want 0", len(empty))
	}

	// Unknown session: ErrNotFound on both append and list.
	if _, err := s.AppendEntry(ctx, "nope", session.EntryUser, p1, nil); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("append unknown: %v, want ErrNotFound", err)
	}
	if _, err := s.ListEntries(ctx, "nope", 0, 0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("list unknown: %v, want ErrNotFound", err)
	}
}

func TestLock(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openDB(t)
	defer closeFn()
	if err := s.CreateSession(ctx, session.SessionMeta{ID: "s1", UserID: "u"}); err != nil {
		t.Fatal(err)
	}

	// Unknown session → ErrNotFound.
	if _, err := s.AcquireLock(ctx, "nope"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("lock unknown: %v, want ErrNotFound", err)
	}

	release, err := s.AcquireLock(ctx, "s1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Second acquisition while held → ErrSessionBusy.
	if _, err := s.AcquireLock(ctx, "s1"); !errors.Is(err, session.ErrSessionBusy) {
		t.Fatalf("re-acquire: %v, want ErrSessionBusy", err)
	}
	release()
	// After release, acquisition succeeds again.
	release2, err := s.AcquireLock(ctx, "s1")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

// TestPersistenceAcrossRestart is the phase-4 crux: data written by one
// "process" (an open *gorm.DB) must be visible to a fresh "process" that
// reopens the same file — this is what lets a restarted server resume an
// interrupted session.
func TestPersistenceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "persist.db")
	dsn := dbPath + "?_busy_timeout=5000"

	// Process #1: create, write entries, mark interrupted, then close.
	db1, err := gorm.Open(sqlite.Open(dsn), cfg())
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	s1 := gormstore.New(db1)
	if err := s1.Migrate(ctx); err != nil {
		t.Fatalf("migrate #1: %v", err)
	}
	if err := s1.CreateSession(ctx, session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m", Status: session.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	pUser := []byte(`{"role":"user","text":"do it"}`)
	pAsst := []byte(`{"role":"assistant","toolCalls":[{"id":"tc9"}]}`)
	if _, err := s1.AppendEntry(ctx, "s1", session.EntryUser, pUser, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AppendEntry(ctx, "s1", session.EntryAssistant, pAsst, nil); err != nil {
		t.Fatal(err)
	}
	meta, _ := s1.GetSession(ctx, "s1")
	meta.Status = session.StatusInterrupted
	if err := s1.UpdateSession(ctx, meta); err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db1.DB(); err == nil && sqlDB != nil {
		if err := sqlDB.Close(); err != nil {
			t.Fatalf("close #1: %v", err)
		}
	}

	// Process #2: reopen the same file with a brand-new store.
	db2, err := gorm.Open(sqlite.Open(dsn), cfg())
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	defer func() {
		if sqlDB, err := db2.DB(); err == nil && sqlDB != nil {
			sqlDB.Close()
		}
	}()
	s2 := gormstore.New(db2)
	if err := s2.Migrate(ctx); err != nil {
		t.Fatalf("migrate #2: %v", err)
	}

	got, err := s2.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status after restart = %q, want interrupted", got.Status)
	}
	entries, err := s2.ListEntries(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("entries after restart: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries after restart = %d, want 2", len(entries))
	}
	if entries[0].Seq != 1 || string(entries[0].Payload) != string(pUser) {
		t.Fatalf("entry[0] after restart = %+v", entries[0])
	}
	if entries[1].Seq != 2 || string(entries[1].Payload) != string(pAsst) {
		t.Fatalf("entry[1] after restart = %+v", entries[1])
	}

	// A new append after restart continues the seq (monotonic across restart).
	seq, err := s2.AppendEntry(ctx, "s1", session.EntryToolResult, []byte(`{"toolCallId":"tc9","error":true}`), nil)
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	if seq != 3 {
		t.Fatalf("seq after restart = %d, want 3 (continued from persisted history)", seq)
	}
}
