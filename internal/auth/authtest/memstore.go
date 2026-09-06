package authtest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/auth"
)

// MemorySessionStore is an in-memory auth.SessionStore keyed by token hash.
// It ignores the tx/Querier handles (they may be nil) and stamps Touch and
// Revoke with its injected clock, mirroring Postgres transaction time.
type MemorySessionStore struct {
	mu       sync.RWMutex
	now      func() time.Time
	byHash   map[string]auth.Session
	byID     map[string]string // id -> token hash
	failNext error
	touches  int
}

// NewMemorySessionStore returns an empty store using now as its clock
// (UTC wall clock when nil).
func NewMemorySessionStore(now func() time.Time) *MemorySessionStore {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &MemorySessionStore{now: now, byHash: map[string]auth.Session{}, byID: map[string]string{}}
}

// FailNext makes the next store call return err (fault injection).
func (s *MemorySessionStore) FailNext(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

// Len returns the number of stored sessions, revoked included.
func (s *MemorySessionStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byHash)
}

// Touches returns how many Touch calls succeeded.
func (s *MemorySessionStore) Touches() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.touches
}

// GetByID returns a stored session by id (test inspection only).
func (s *MemorySessionStore) GetByID(id string) (auth.Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.byID[id]
	if !ok {
		return auth.Session{}, false
	}
	return clone(s.byHash[h]), true
}

func (s *MemorySessionStore) injected() error {
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	return nil
}

// Create implements auth.SessionStore.
func (s *MemorySessionStore) Create(_ context.Context, _ pgx.Tx, sess auth.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected(); err != nil {
		return err
	}
	if _, dup := s.byHash[sess.TokenHash]; dup {
		return auth.ErrSessionExists
	}
	if _, dup := s.byID[sess.ID]; dup {
		return auth.ErrSessionExists
	}
	s.byHash[sess.TokenHash] = clone(sess)
	s.byID[sess.ID] = sess.TokenHash
	return nil
}

// Get implements auth.SessionStore.
func (s *MemorySessionStore) Get(_ context.Context, _ auth.Querier, tokenHash string) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected(); err != nil {
		return auth.Session{}, err
	}
	sess, ok := s.byHash[tokenHash]
	if !ok {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	return clone(sess), nil
}

// Touch implements auth.SessionStore.
func (s *MemorySessionStore) Touch(_ context.Context, _ auth.Querier, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected(); err != nil {
		return err
	}
	h, ok := s.byID[id]
	if !ok {
		return auth.ErrSessionNotFound
	}
	sess := s.byHash[h]
	sess.LastSeenAt = s.now().UTC()
	s.byHash[h] = sess
	s.touches++
	return nil
}

// Revoke implements auth.SessionStore. It is idempotent.
func (s *MemorySessionStore) Revoke(_ context.Context, _ auth.Querier, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected(); err != nil {
		return err
	}
	h, ok := s.byID[id]
	if !ok {
		return auth.ErrSessionNotFound
	}
	sess := s.byHash[h]
	if sess.RevokedAt == nil {
		t := s.now().UTC()
		sess.RevokedAt = &t
		s.byHash[h] = sess
	}
	return nil
}

// RevokeAllForSubject implements auth.SessionStore.
func (s *MemorySessionStore) RevokeAllForSubject(_ context.Context, _ auth.Querier, subjectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected(); err != nil {
		return 0, err
	}
	n := 0
	t := s.now().UTC()
	for h, sess := range s.byHash {
		if sess.SubjectID == subjectID && sess.RevokedAt == nil {
			rt := t
			sess.RevokedAt = &rt
			s.byHash[h] = sess
			n++
		}
	}
	return n, nil
}

// ListForSubject implements auth.SessionStore: newest first.
func (s *MemorySessionStore) ListForSubject(_ context.Context, _ auth.Querier, subjectID string) ([]auth.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.injected(); err != nil {
		return nil, err
	}
	var out []auth.Session
	for _, sess := range s.byHash {
		if sess.SubjectID == subjectID {
			out = append(out, clone(sess))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func clone(s auth.Session) auth.Session {
	c := s
	c.Roles = append(c.Roles[:0:0], s.Roles...)
	c.AccountIDs = append(c.AccountIDs[:0:0], s.AccountIDs...)
	c.AMR = append(c.AMR[:0:0], s.AMR...)
	if s.RevokedAt != nil {
		t := *s.RevokedAt
		c.RevokedAt = &t
	}
	if s.BreakGlassUntil != nil {
		t := *s.BreakGlassUntil
		c.BreakGlassUntil = &t
	}
	return c
}
