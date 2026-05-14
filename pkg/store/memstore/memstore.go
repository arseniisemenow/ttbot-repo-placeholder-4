// Package memstore is an in-memory store.Store for the identity bot. Used by
// tests and as the cold-start fallback in the function entrypoint.
package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	s21account "github.com/arseniisemenow/s21-account-go"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store"
)

// Store is an in-memory store.
type Store struct {
	mu             sync.Mutex
	admin          *store.BotAdmin // nil = unset
	pendingDeletes map[pendingKey]store.PendingDelete
	s21Accounts    map[int64]store.S21Account
}

type pendingKey struct {
	ChatID, MessageID int64
}

// New returns an empty memstore.
func New() *Store {
	return &Store{
		pendingDeletes: map[pendingKey]store.PendingDelete{},
		s21Accounts:    map[int64]store.S21Account{},
	}
}

// Close is a no-op.
func (s *Store) Close() error { return nil }

func (s *Store) Admin() store.AdminRepo                   { return adminRepo{s} }
func (s *Store) PendingDeletes() store.PendingDeleteRepo  { return pendingRepo{s} }
func (s *Store) S21Accounts() store.S21AccountRepo        { return s21AccountRepo{s} }

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

type s21AccountRepo struct{ s *Store }

func (r s21AccountRepo) Get(_ context.Context, tid int64) (store.S21Account, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	a, ok := r.s.s21Accounts[tid]
	if !ok {
		return store.S21Account{}, s21account.ErrNotFound
	}
	return a, nil
}

func (r s21AccountRepo) List(_ context.Context) ([]store.S21Account, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]store.S21Account, 0, len(r.s.s21Accounts))
	for _, a := range r.s.s21Accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].TelegramID < out[j].TelegramID
	})
	return out, nil
}

func (r s21AccountRepo) Upsert(_ context.Context, a store.S21Account) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if existing, ok := r.s.s21Accounts[a.TelegramID]; ok && a.CreatedAt.IsZero() {
		a.CreatedAt = existing.CreatedAt
	}
	r.s.s21Accounts[a.TelegramID] = a
	return nil
}

func (r s21AccountRepo) Delete(_ context.Context, tid int64) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	delete(r.s.s21Accounts, tid)
	return nil
}
