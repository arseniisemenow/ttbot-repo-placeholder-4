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
	Close() error
}

// AdminRepo persists the single admin row.
type AdminRepo interface {
	Get(ctx context.Context) (BotAdmin, error)
	Set(ctx context.Context, a BotAdmin) error
}
