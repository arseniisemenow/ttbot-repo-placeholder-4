package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/messenger"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/s21"
)

// PeriodicJob is the body of identity-bot-cron. Two responsibilities:
//
//  1. Re-validate the primary admin's stored S21 credentials against S21.
//     If S21 rejects them (password rotated, account disabled), DM the admin
//     so they know to re-run /admin. Transport errors are swallowed — only
//     ErrInvalidCredentials triggers the notification.
//
//  2. Sweep the pending_deletes table, removing each due message from
//     Telegram and from the table. Best-effort: failures are logged but
//     don't abort the rest of the pass.
//
// Both run sequentially; failures in one don't prevent the other.
func (h *Handlers) PeriodicJob(ctx context.Context) error {
	if err := h.checkAdminCreds(ctx); err != nil {
		log.Printf("periodic: check admin creds: %v", err)
	}
	if err := h.sweepPendingDeletes(ctx); err != nil {
		log.Printf("periodic: sweep pending deletes: %v", err)
	}
	return nil
}

// checkAdminCreds tries to re-authenticate the stored admin against S21.
// On ErrInvalidCredentials, DM the admin asking them to re-run /admin. We
// don't touch the stored row — the admin re-runs /admin themselves once
// they have a working password, which overwrites it with last-wins semantics.
func (h *Handlers) checkAdminCreds(ctx context.Context) error {
	admin, err := h.Store.Admin().Get(ctx)
	if err != nil {
		return err // ErrNotFound is fine here — bot has no admin yet
	}
	password, err := h.Cipher.Decrypt(admin.S21CredsEncrypted)
	if err != nil {
		// Encryption-key mismatch (operator rotated the key without
		// re-encrypting). The admin will need to re-run /admin manually;
		// nothing automated can recover from this.
		_, _ = h.M.SendMessage(ctx, admin.TelegramID,
			"Internal: I can't decrypt your stored S21 credentials. Please re-run /admin <login>:<password>.")
		return fmt.Errorf("decrypt admin creds for tid=%d: %w", admin.TelegramID, err)
	}
	_, err = h.S21.Authenticate(ctx, admin.S21Login, password)
	switch {
	case errors.Is(err, s21.ErrInvalidCredentials):
		// Notify the specific admin who provided the stale creds.
		msg := fmt.Sprintf(
			"Heads up: S21 rejected your stored credentials for login %q. Please re-run /admin <login>:<password> so I can keep validating nicknames.",
			admin.S21Login)
		if _, sendErr := h.M.SendMessage(ctx, admin.TelegramID, msg); sendErr != nil {
			log.Printf("notify stale-creds admin tid=%d: %v", admin.TelegramID, sendErr)
		}
		return nil
	case err != nil:
		// Transport / S21-outage error — don't notify; will retry next tick.
		return err
	}
	return nil
}

// sweepPendingDeletes deletes every due message and removes the row. A
// Telegram delete failing with "message to delete not found" (the user
// already deleted it themselves) still drops the row so it doesn't pile up.
func (h *Handlers) sweepPendingDeletes(ctx context.Context) error {
	due, err := h.Store.PendingDeletes().ListDue(ctx, h.Cfg.Now().UTC())
	if err != nil {
		return err
	}
	for _, p := range due {
		err := h.M.DeleteMessage(ctx, p.ChatID, p.MessageID)
		if err != nil && !errors.Is(err, messenger.ErrNotFound) {
			log.Printf("delete chat=%d msg=%d: %v", p.ChatID, p.MessageID, err)
			continue
		}
		if err := h.Store.PendingDeletes().Delete(ctx, p.ChatID, p.MessageID); err != nil {
			log.Printf("remove pending_delete row chat=%d msg=%d: %v", p.ChatID, p.MessageID, err)
		}
	}
	return nil
}
