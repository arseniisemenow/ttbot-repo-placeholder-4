// Package memstore is an in-memory store.Store for the identity bot. Used by
// tests and as the cold-start fallback in the function entrypoint.
package memstore

import (
	"context"
	"sync"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store"
)

// Store is an in-memory store.
type Store struct {
	mu    sync.Mutex
	admin *store.BotAdmin // nil = unset
}

// New returns an empty memstore.
func New() *Store { return &Store{} }

// Close is a no-op.
func (s *Store) Close() error { return nil }

func (s *Store) Admin() store.AdminRepo { return adminRepo{s} }

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
