package handlers

import (
	"context"
	"errors"

	s21account "github.com/arseniisemenow/s21-account-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/store"
)

// MigrateBotAdminToS21Accounts is the one-shot bootstrap migration: copy the
// legacy single-row bot_admin table into the new s21_accounts table.
//
// Behavior:
//   - bot_admin empty → no-op.
//   - bot_admin row already mirrored in s21_accounts → no-op.
//   - Otherwise upsert. CampusID/CampusName come over empty (the legacy table
//     didn't store them); the next /login by the same user, or the next cron
//     probe, will not auto-populate them — only an explicit /login can. That
//     is acceptable: /whoami still works, and the row is still picked by
//     PickHealthy for identity-service calls.
//
// Idempotent and safe to call on every cold start. We keep the bot_admin
// table around so a rollback is possible until we are confident the new
// path is healthy; a later deploy can drop the schema resource.
func MigrateBotAdminToS21Accounts(ctx context.Context, st store.Store) error {
	legacy, err := st.Admin().Get(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := st.S21Accounts().Get(ctx, legacy.TelegramID); err == nil {
		return nil
	} else if !errors.Is(err, s21account.ErrNotFound) {
		return err
	}
	return st.S21Accounts().Upsert(ctx, s21account.S21Account{
		TelegramID:           legacy.TelegramID,
		S21Login:             legacy.S21Login,
		S21CredsEncrypted:    legacy.S21CredsEncrypted,
		CreatedAt:            legacy.UpdatedAt, // legacy row has no separate CreatedAt — use UpdatedAt as a reasonable proxy
		UpdatedAt:            legacy.UpdatedAt,
		S21CredsFailedAt:     legacy.S21CredsFailedAt,
		S21CredsLastWarnedAt: legacy.S21CredsLastWarnedAt,
	})
}
