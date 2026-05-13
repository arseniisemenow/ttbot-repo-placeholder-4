// Package store defines the identity-bot's storage interface. The bot holds
// one row: the current admin's encrypted S21 credentials.
package store

import (
	"context"
	"errors"
	"time"
)

// BotAdmin is the (one-row) bot_admin table. Last-wins: a successful /admin
// call replaces this row.
type BotAdmin struct {
	TelegramID         int64
	S21Login           string
	S21CredsEncrypted  string
	UpdatedAt          time.Time
}

// ErrNotFound is returned when the bot has no admin set yet.
var ErrNotFound = errors.New("store: not found")

// Store is the union of repos.
type Store interface {
	Admin() AdminRepo
	PendingDeletes() PendingDeleteRepo
	Close() error
}

// AdminRepo persists the single admin row.
type AdminRepo interface {
	Get(ctx context.Context) (BotAdmin, error)
	Set(ctx context.Context, a BotAdmin) error
}

// PendingDelete is one row in pending_deletes — a message the bot has DMed
// that needs to be removed at or after DeleteAt. The /new_read_key flow uses
// this to vanish the plaintext-key DM ~15 minutes after issue.
type PendingDelete struct {
	ChatID    int64
	MessageID int64
	DeleteAt  time.Time
	CreatedAt time.Time
}

// PendingDeleteRepo persists messages awaiting deferred deletion.
type PendingDeleteRepo interface {
	// Insert schedules a delete. If a row already exists for the same
	// (ChatID, MessageID), it is overwritten — the new DeleteAt wins.
	Insert(ctx context.Context, p PendingDelete) error
	// ListDue returns every row whose DeleteAt is <= now, ordered by
	// DeleteAt ascending. Used by the cron sweep.
	ListDue(ctx context.Context, now time.Time) ([]PendingDelete, error)
	// Delete removes a row by (ChatID, MessageID). Idempotent — returns nil
	// when no row matched.
	Delete(ctx context.Context, chatID, messageID int64) error
}
