// Package memstore is an in-memory store.Store for the identity bot. Used by
// tests and as the cold-start fallback in the function entrypoint.
package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store"
)

// Store is an in-memory store.
type Store struct {
	mu             sync.Mutex
	admin          *store.BotAdmin // nil = unset
	pendingDeletes map[pendingKey]store.PendingDelete
}

type pendingKey struct {
	ChatID, MessageID int64
}

// New returns an empty memstore.
func New() *Store {
	return &Store{
		pendingDeletes: map[pendingKey]store.PendingDelete{},
	}
}

// Close is a no-op.
func (s *Store) Close() error { return nil }

func (s *Store) Admin() store.AdminRepo                   { return adminRepo{s} }
func (s *Store) PendingDeletes() store.PendingDeleteRepo  { return pendingRepo{s} }

type adminRepo struct{ s *Store }

func (r adminRepo) Get(_ context.Context) (store.BotAdmin, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.admin == nil {
		return store.BotAdmin{}, store.ErrNotFound
	}
	return *r.s.admin, nil
}

func (r adminRepo) Set(_ context.Context, a store.BotAdmin) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.admin = &a
	return nil
}

func (r adminRepo) Delete(_ context.Context) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.admin = nil
	return nil
}

type pendingRepo struct{ s *Store }

func (r pendingRepo) Insert(_ context.Context, p store.PendingDelete) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	r.s.pendingDeletes[pendingKey{p.ChatID, p.MessageID}] = p
	return nil
}

func (r pendingRepo) ListDue(_ context.Context, now time.Time) ([]store.PendingDelete, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []store.PendingDelete
	for _, p := range r.s.pendingDeletes {
		if !p.DeleteAt.After(now) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeleteAt.Before(out[j].DeleteAt) })
	return out, nil
}

func (r pendingRepo) Delete(_ context.Context, chatID, messageID int64) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	delete(r.s.pendingDeletes, pendingKey{chatID, messageID})
	return nil
}
