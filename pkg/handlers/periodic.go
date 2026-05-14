package handlers

import (
	"context"
	"errors"
	"log"

	s21account "github.com/arseniisemenow/s21-account-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
)

// PeriodicJob is the body of identity-bot-cron. Two responsibilities:
//
//  1. Probe every s21_accounts row's stored creds against S21 once. The
//     shared package's ApplyAuthResult turns the auth outcome into a
//     Decision (persist markers, warn at 1d/3d/6d milestones, delete the
//     row + final DM at the 7d deadline).
//  2. Sweep the pending_deletes table — delete each due message from
//     Telegram and remove the row.
//
// Per-row errors are logged and skipped so one bad row doesn't abort the
// rest.
func (h *Handlers) PeriodicJob(ctx context.Context) error {
	if err := h.probeAllAccounts(ctx); err != nil {
		log.Printf("periodic: probe accounts: %v", err)
	}
	if err := h.sweepPendingDeletes(ctx); err != nil {
		log.Printf("periodic: sweep pending deletes: %v", err)
	}
	return nil
}

// probeAllAccounts walks every s21_accounts row, authenticates each row's
// creds, and applies the shared package's Decision.
func (h *Handlers) probeAllAccounts(ctx context.Context) error {
	rows, err := h.Store.S21Accounts().List(ctx)
	if err != nil {
		return err
	}
	adapter := s21ClientAdapter{inner: h.S21}
	for _, a := range rows {
		h.probeOne(ctx, adapter, a)
	}
	return nil
}

func (h *Handlers) probeOne(ctx context.Context, adapter s21ClientAdapter, a s21account.S21Account) {
	password, err := h.Cipher.Decrypt(a.S21CredsEncrypted)
	if err != nil {
		log.Printf("decrypt creds tid=%d: %v", a.TelegramID, err)
		_, _ = h.M.SendMessage(ctx, a.TelegramID,
			"Internal: I can't decrypt your stored S21 credentials. Please /logout and /login again.")
		return
	}
	_, authErr := adapter.Authenticate(ctx, a.S21Login, password)
	d := s21account.ApplyAuthResult(a, authErr, h.Cfg.Now())
	if d.Logout {
		if err := h.Store.S21Accounts().Delete(ctx, a.TelegramID); err != nil {
			log.Printf("auto-logout delete tid=%d: %v", a.TelegramID, err)
			return
		}
		if _, err := h.M.SendMessage(ctx, a.TelegramID, d.LogoutDM); err != nil {
			log.Printf("auto-logout DM tid=%d: %v", a.TelegramID, err)
		}
		log.Printf("auto-logout: cleared tid=%d login=%q", a.TelegramID, a.S21Login)
		return
	}
	if d.PersistUpdate {
		if err := h.Store.S21Accounts().Upsert(ctx, d.UpdatedAccount); err != nil {
			log.Printf("upsert account tid=%d: %v", a.TelegramID, err)
		}
	}
	if d.WarningDM != "" {
		if _, err := h.M.SendMessage(ctx, a.TelegramID, d.WarningDM); err != nil {
			log.Printf("warning DM tid=%d: %v", a.TelegramID, err)
		}
	}
}

// sweepPendingDeletes deletes every due message and removes the row. A
// "message to delete not found" error (the user deleted it themselves)
// still drops the row so it doesn't pile up.
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
