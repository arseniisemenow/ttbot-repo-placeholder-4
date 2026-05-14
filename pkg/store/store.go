// Package store defines the identity-bot's storage interface. The bot holds
// one row: the current admin's encrypted S21 credentials.
package store

import (
	"context"
	"errors"
	"time"

	s21account "github.com/arseniisemenow/s21-account-go"
)

// BotAdmin is the (one-row) bot_admin table. First-wins-by-identity: a
// successful /admin call by the same telegram_id rotates the row; a
// different telegram_id is rejected. Step-down via /unadmin OR auto-unadmin
// (see S21CredsFailedAt).
type BotAdmin struct {
	TelegramID        int64
	S21Login          string
	S21CredsEncrypted string
	UpdatedAt         time.Time
	// S21CredsFailedAt is the timestamp of the FIRST failed re-auth in the
	// current failure run (cleared on next successful auth). nil = healthy.
	// The cron uses this to drive the 7-day auto-unadmin clock.
	S21CredsFailedAt *time.Time
	// S21CredsLastWarnedAt is the timestamp of the last warning DM sent
	// during the current failure run. Used to schedule the four-DM cadence
	// (first failure / 1d / 3d / 6d) without spamming on every 15-min tick.
	S21CredsLastWarnedAt *time.Time
}

// ErrNotFound is returned when the bot has no admin set yet.
var ErrNotFound = errors.New("store: not found")

// Store is the union of repos.
type Store interface {
	Admin() AdminRepo
	PendingDeletes() PendingDeleteRepo
	S21Accounts() S21AccountRepo
	Close() error
}

// S21Account is re-exported from the shared package so the rest of the bot
// uses one canonical type.
type S21Account = s21account.S21Account

// S21AccountRepo persists logged-in accounts. The shape matches
// s21account.Store exactly (so any S21AccountRepo also satisfies that
// interface). List MUST return rows ordered by created_at ASC.
type S21AccountRepo interface {
	Get(ctx context.Context, telegramID int64) (s21account.S21Account, error)
	List(ctx context.Context) ([]s21account.S21Account, error)
	Upsert(ctx context.Context, a s21account.S21Account) error
	Delete(ctx context.Context, telegramID int64) error
}

// AdminRepo persists the single admin row.
type AdminRepo interface {
	Get(ctx context.Context) (BotAdmin, error)
	Set(ctx context.Context, a BotAdmin) error
	// Delete clears the admin row. Idempotent: deleting when no row exists
	// returns nil. Used by /unadmin to let the current admin step down.
	Delete(ctx context.Context) error
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
