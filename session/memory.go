package session

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the in-memory reference implementation of Store: it backs
// the unit tests, the hello example, and phase 1 before the GORM adapter
// lands (phase 4).
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]SessionMeta
	entries  map[string][]Entry
	nextSeq  map[string]int64
	locked   map[string]bool
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions: make(map[string]SessionMeta),
		entries:  make(map[string][]Entry),
		nextSeq:  make(map[string]int64),
		locked:   make(map[string]bool),
	}
}

func (s *MemoryStore) CreateSession(_ context.Context, meta SessionMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta.ID == "" {
		return ErrInvalidMeta
	}
	if _, ok := s.sessions[meta.ID]; ok {
		return ErrSessionExists
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	s.sessions[meta.ID] = meta
	return nil
}

func (s *MemoryStore) GetSession(_ context.Context, id string) (SessionMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.sessions[id]
	if !ok {
		return SessionMeta{}, ErrNotFound
	}
	return meta, nil
}

func (s *MemoryStore) UpdateSession(_ context.Context, meta SessionMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[meta.ID]; !ok {
		return ErrNotFound
	}
	meta.UpdatedAt = time.Now()
	s.sessions[meta.ID] = meta
	return nil
}

func (s *MemoryStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(s.sessions, id)
	delete(s.entries, id)
	delete(s.nextSeq, id)
	delete(s.locked, id)
	return nil
}

func (s *MemoryStore) ListSessions(_ context.Context, q SessionQuery) ([]SessionMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SessionMeta, 0, len(s.sessions))
	for _, m := range s.sessions {
		if q.UserID != "" && m.UserID != q.UserID {
			continue
		}
		if q.Agent != "" && m.Agent != q.Agent {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *MemoryStore) AppendEntry(_ context.Context, sessionID string, typ EntryType, payload, usage json.RawMessage) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return 0, ErrNotFound
	}
	s.nextSeq[sessionID]++
	e := Entry{
		Seq:       s.nextSeq[sessionID],
		SessionID: sessionID,
		Type:      typ,
		Payload:   append(json.RawMessage(nil), payload...),
		Usage:     append(json.RawMessage(nil), usage...),
		CreatedAt: time.Now(),
	}
	s.entries[sessionID] = append(s.entries[sessionID], e)
	return e.Seq, nil
}

func (s *MemoryStore) ListEntries(_ context.Context, sessionID string, afterSeq int64, limit int) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// A session with no entries yet is valid — return an empty list, not
	// ErrNotFound (the entries map is only populated on first append).
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	all := s.entries[sessionID]
	out := make([]Entry, 0, len(all))
	for _, e := range all {
		if e.Seq <= afterSeq {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) AcquireLock(_ context.Context, sessionID string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	if s.locked[sessionID] {
		return nil, ErrSessionBusy
	}
	s.locked[sessionID] = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.locked, sessionID)
	}, nil
}
