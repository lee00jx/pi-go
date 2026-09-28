// Package gorm is the official persistent adapter for session.Store (DESIGN
// §6, phase 4). It backs the store with a relational database via GORM so
// sessions and their append-only entries survive a process restart — enabling
// the phase-4 recovery story (kill -TERM → restart → resume).
//
// Driver-agnostic by design: New takes an already-opened *gorm.DB, so the
// sqlite/mysql/postgres driver is chosen by the caller (see the gin example and
// the tests in this package). The core packages (core/provider/session/
// gateway/tools) never import this package; integrators who only use the
// in-memory store keep a zero-framework-dependency footprint.
//
// The per-session run lock (AcquireLock) is deliberately ephemeral and
// in-process: it only guards a single RunLoop per session inside the running
// gateway. It must NOT survive a crash, or an orphaned lock would block
// resume. The persistent part (sessions + entries) is what survives a restart.
package gorm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/lee00jx/pi-go/session"
)

// SessionModel is the persisted session row (table: sessions).
type SessionModel struct {
	ID        string `gorm:"primaryKey"`
	UserID    string `gorm:"index;index:idx_sessions_user_agent,priority:1"`
	Agent     string `gorm:"index:idx_sessions_user_agent,priority:2"`
	Provider  string
	Model     string
	Status    string
	Title     string
	TokensIn  int64
	TokensOut int64
	Cost      float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EntryModel is one append-only entry row (table: entries). (session_id, seq)
// is unique and seq is monotonic per session — the basis for SSE replay and
// reconnect catch-up (DESIGN §6.5).
type EntryModel struct {
	ID        uint64 `gorm:"primaryKey;autoIncrement"`
	SessionID string `gorm:"uniqueIndex:idx_session_seq,priority:1"`
	Seq       int64  `gorm:"uniqueIndex:idx_session_seq,priority:2"`
	Type      string
	Payload   []byte
	Usage     []byte
	CreatedAt time.Time
}

// GORMStore implements session.Store on top of GORM.
type GORMStore struct {
	db    *gorm.DB
	mu    sync.Mutex
	locks map[string]struct{}
}

var _ session.Store = (*GORMStore)(nil)

// New wraps an already-opened *gorm.DB. Call Migrate before first use.
func New(db *gorm.DB) *GORMStore {
	return &GORMStore{db: db, locks: make(map[string]struct{})}
}

// Migrate creates the tables if they do not exist.
func (s *GORMStore) Migrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&SessionModel{}, &EntryModel{})
}

func (s *GORMStore) CreateSession(ctx context.Context, meta session.SessionMeta) error {
	if meta.ID == "" {
		return session.ErrInvalidMeta
	}
	// Pre-check so the ErrSessionExists contract holds regardless of the
	// caller's GORM TranslateError setting; the primary key is the DB-level
	// guarantee, and the errors.Is fallback below covers the create race.
	var n int64
	if err := s.db.WithContext(ctx).Model(&SessionModel{}).Where("id = ?", meta.ID).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return session.ErrSessionExists
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	m := toModel(meta)
	res := s.db.WithContext(ctx).Create(&m)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
			return session.ErrSessionExists
		}
		return res.Error
	}
	return nil
}

func (s *GORMStore) GetSession(ctx context.Context, id string) (session.SessionMeta, error) {
	var m SessionModel
	if err := s.db.WithContext(ctx).First(&m, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return session.SessionMeta{}, session.ErrNotFound
		}
		return session.SessionMeta{}, err
	}
	return fromModel(m), nil
}

func (s *GORMStore) UpdateSession(ctx context.Context, meta session.SessionMeta) error {
	meta.UpdatedAt = time.Now()
	// Agent is immutable after create and is intentionally omitted so a
	// Get-then-mutate caller cannot wipe the type (and MemoryStore's
	// full-replace counterpart must Get first for the same reason).
	res := s.db.WithContext(ctx).Model(&SessionModel{}).Where("id = ?", meta.ID).
		Updates(map[string]interface{}{
			"user_id": meta.UserID, "provider": meta.Provider, "model": meta.Model,
			"status": string(meta.Status), "title": meta.Title,
			"tokens_in": meta.TokensIn, "tokens_out": meta.TokensOut, "cost": meta.Cost,
			"updated_at": meta.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return session.ErrNotFound
	}
	return nil
}

func (s *GORMStore) DeleteSession(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&SessionModel{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return session.ErrNotFound
	}
	// Entries are append-only and cascade with the session (no DB FK to stay
	// driver-agnostic; remove explicitly).
	if err := s.db.WithContext(ctx).Where("session_id = ?", id).Delete(&EntryModel{}).Error; err != nil {
		return err
	}
	return nil
}

func (s *GORMStore) ListSessions(ctx context.Context, q session.SessionQuery) ([]session.SessionMeta, error) {
	db := s.db.WithContext(ctx).Model(&SessionModel{}).Order("updated_at DESC")
	if q.UserID != "" {
		db = db.Where("user_id = ?", q.UserID)
	}
	if q.Agent != "" {
		db = db.Where("agent = ?", q.Agent)
	}
	if q.Limit > 0 {
		db = db.Limit(q.Limit)
	}
	var ms []SessionModel
	if err := db.Find(&ms).Error; err != nil {
		return nil, err
	}
	out := make([]session.SessionMeta, 0, len(ms))
	for _, m := range ms {
		out = append(out, fromModel(m))
	}
	return out, nil
}

// AppendEntry appends one entry and returns its assigned seq. Seq is computed
// in a transaction as MAX(seq)+1 for the session; the per-session run lock
// (held by the gateway around a whole RunLoop) serializes writers, so the
// read-then-insert cannot collide.
func (s *GORMStore) AppendEntry(ctx context.Context, sessionID string, typ session.EntryType, payload, usage json.RawMessage) (int64, error) {
	var seq int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sm SessionModel
		if err := tx.First(&sm, "id = ?", sessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return session.ErrNotFound
			}
			return err
		}
		var maxSeq int64
		row := tx.Model(&EntryModel{}).
			Where("session_id = ?", sessionID).
			Select("COALESCE(MAX(seq), 0)").
			Row()
		if err := row.Scan(&maxSeq); err != nil {
			return err
		}
		seq = maxSeq + 1
		return tx.Create(&EntryModel{
			SessionID: sessionID,
			Seq:       seq,
			Type:      string(typ),
			Payload:   []byte(payload),
			Usage:     []byte(usage),
			CreatedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *GORMStore) ListEntries(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]session.Entry, error) {
	// A session with no entries yet is valid — return an empty list, not
	// ErrNotFound (mirrors the in-memory reference implementation).
	var sm SessionModel
	if err := s.db.WithContext(ctx).First(&sm, "id = ?", sessionID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}
	q := s.db.WithContext(ctx).Model(&EntryModel{}).
		Where("session_id = ? AND seq > ?", sessionID, afterSeq).
		Order("seq ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var es []EntryModel
	if err := q.Find(&es).Error; err != nil {
		return nil, err
	}
	out := make([]session.Entry, 0, len(es))
	for _, e := range es {
		out = append(out, toEntry(e))
	}
	return out, nil
}

// AcquireLock guards a single RunLoop per session within the running process.
// It is in-memory on purpose (see the package comment): an ephemeral lock frees
// automatically when the process dies, so a restart can resume a session
// without clearing a stale row.
func (s *GORMStore) AcquireLock(ctx context.Context, sessionID string) (release func(), err error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&SessionModel{}).Where("id = ?", sessionID).Count(&n).Error; err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, session.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.locks[sessionID]; ok {
		return nil, session.ErrSessionBusy
	}
	s.locks[sessionID] = struct{}{}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.locks, sessionID)
	}, nil
}

func toModel(m session.SessionMeta) SessionModel {
	return SessionModel{
		ID: m.ID, UserID: m.UserID, Agent: m.Agent,
		Provider: m.Provider, Model: m.Model,
		Status: string(m.Status), Title: m.Title,
		TokensIn: m.TokensIn, TokensOut: m.TokensOut, Cost: m.Cost,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func fromModel(m SessionModel) session.SessionMeta {
	return session.SessionMeta{
		ID: m.ID, UserID: m.UserID, Agent: m.Agent,
		Provider: m.Provider, Model: m.Model,
		Status: session.SessionStatus(m.Status), Title: m.Title,
		TokensIn: m.TokensIn, TokensOut: m.TokensOut, Cost: m.Cost,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func toEntry(e EntryModel) session.Entry {
	return session.Entry{
		Seq: e.Seq, SessionID: e.SessionID, Type: session.EntryType(e.Type),
		Payload: json.RawMessage(e.Payload), Usage: json.RawMessage(e.Usage),
		CreatedAt: e.CreatedAt,
	}
}
