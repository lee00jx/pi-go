package session

import (
	"context"
	"testing"
	"time"
)

func TestMemoryListSessionsQuery(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	mk := func(id, user, agent string) {
		t.Helper()
		if err := s.CreateSession(ctx, SessionMeta{
			ID: id, UserID: user, Agent: agent, Provider: "faux", Model: "m",
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mk("c1", "alice", "calc")
	mk("p1", "alice", "compare")
	mk("c2", "bob", "calc")

	alice, err := s.ListSessions(ctx, SessionQuery{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 2 || alice[0].ID != "p1" || alice[1].ID != "c1" {
		t.Fatalf("alice = %+v, want p1 then c1", alice)
	}

	calc, err := s.ListSessions(ctx, SessionQuery{UserID: "alice", Agent: "calc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(calc) != 1 || calc[0].ID != "c1" {
		t.Fatalf("alice calc = %+v, want c1", calc)
	}

	limited, err := s.ListSessions(ctx, SessionQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].ID != "c2" {
		t.Fatalf("limit = %+v, want newest c2", limited)
	}
}

func TestMemoryUpdatePreservesAgentWhenGetFirst(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateSession(ctx, SessionMeta{
		ID: "s1", UserID: "u", Agent: "calc", Provider: "faux", Model: "m",
	}); err != nil {
		t.Fatal(err)
	}
	meta, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	meta.Status = StatusRunning
	if err := s.UpdateSession(ctx, meta); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, "s1")
	if got.Agent != "calc" || got.Status != StatusRunning {
		t.Fatalf("got %+v, want agent=calc running", got)
	}
}
