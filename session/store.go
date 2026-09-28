// Package session defines the storage contract (Store), session metadata
// and the in-memory reference implementation. Concrete backends implement
// Store: the official GORM adapter (adapter/gorm, phase 4) or integrator
// code (ADR-001).
//
// Core principle (DESIGN §6.1): what is stored ≠ what is fed. Entries are
// appended and never rewritten; the working context is derived from them
// each turn (core.buildContext).
package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Entry types persisted per session (DESIGN §6.2).
type EntryType string

const (
	EntryUser       EntryType = "user"
	EntryAssistant  EntryType = "assistant"
	EntryToolResult EntryType = "tool_result"
	EntryCompaction EntryType = "compaction"
	EntryUIEvent    EntryType = "ui_event"
)

// Session lifecycle statuses (DESIGN §6.2).
type SessionStatus string

const (
	StatusIdle        SessionStatus = "idle"
	StatusRunning     SessionStatus = "running"
	StatusInterrupted SessionStatus = "interrupted"
	StatusEnded       SessionStatus = "ended"
)

// Sentinel errors.
var (
	ErrNotFound      = errors.New("session: not found")
	ErrSessionExists = errors.New("session: already exists")
	ErrSessionBusy   = errors.New("session: another run is in progress")
	ErrInvalidMeta   = errors.New("session: invalid metadata")
)

// SessionMeta is the per-session header row.
type SessionMeta struct {
	ID        string        `json:"id"`
	UserID    string        `json:"userId"`
	Provider  string        `json:"provider"`
	Model     string        `json:"model"` // may be changed mid-session (DESIGN §3.5)
	Status    SessionStatus `json:"status"`
	Title     string        `json:"title,omitempty"`
	Agent     string        `json:"agent,omitempty"` // opaque type; immutable after create
	TokensIn  int64         `json:"tokensIn"`
	TokensOut int64         `json:"tokensOut"`
	Cost      float64       `json:"cost"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

// SessionQuery filters ListSessions. HTTP always sets UserID to the
// authenticated caller; UserID "" is tests/internal (list everyone).
// Agent "" means do not filter by type. Limit 0 means no cap.
type SessionQuery struct {
	UserID string
	Agent  string
	Limit  int
}

// Entry is one append-only row. Payload carries the full message / event /
// compaction JSON; Usage is set for assistant entries.
type Entry struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"sessionId"`
	Type      EntryType       `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Store is the public storage contract (semver-managed). Implementations
// must be safe for concurrent use. Seq is assigned by the store and is
// monotonic per session — it is the basis for SSE replay and reconnect
// catch-up (DESIGN §6.5).
type Store interface {
	CreateSession(ctx context.Context, meta SessionMeta) error
	GetSession(ctx context.Context, id string) (SessionMeta, error)
	UpdateSession(ctx context.Context, meta SessionMeta) error
	DeleteSession(ctx context.Context, id string) error
	// ListSessions returns sessions matching q, newest first.
	ListSessions(ctx context.Context, q SessionQuery) ([]SessionMeta, error)

	// AppendEntry appends one entry and returns its assigned seq.
	AppendEntry(ctx context.Context, sessionID string, typ EntryType, payload, usage json.RawMessage) (int64, error)
	// ListEntries returns entries with seq > afterSeq in ascending order,
	// at most limit entries (0 = all).
	ListEntries(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]Entry, error)

	// AcquireLock guarantees a single RunLoop per session (DESIGN §7.2).
	// It returns ErrSessionBusy when another run holds the lock; the
	// returned release function must always be called.
	AcquireLock(ctx context.Context, sessionID string) (release func(), err error)
}
