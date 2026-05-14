package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/messenger"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/s21"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store"
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

// Warning cadence: a DM is sent on first failure, then again after these
// elapsed milestones since the first failure. At autoUnadminAfter the row
// is deleted and a final DM goes out. Cached nicknames keep /provide_nickname
// working until then; only fresh S21 validations break.
var warningMilestones = []time.Duration{
	24 * time.Hour,      // 1d
	3 * 24 * time.Hour,  // 3d
	6 * 24 * time.Hour,  // 6d
}

const autoUnadminAfter = 7 * 24 * time.Hour

// checkAdminCreds re-authenticates the stored admin against S21 once per cron
// tick. Behaviour:
//
//   - Success: clear any in-flight failure markers (S21CredsFailedAt /
//     S21CredsLastWarnedAt). Silent.
//   - Transport error (S21 outage etc.): no change; retried next tick.
//   - ErrInvalidCredentials:
//   - First failure of a new run: stamp S21CredsFailedAt, stamp
//     S21CredsLastWarnedAt, DM the admin once.
//   - Subsequent failures: DM only when the next milestone (1d / 3d / 6d)
//     has been crossed since the last warning.
//   - After 7d of continuous failure: delete the admin row, DM a final
//     notice. /provide_nickname now relies on the cached nickname index
//     (no auto-failure for cached lookups).
func (h *Handlers) checkAdminCreds(ctx context.Context) error {
	admin, err := h.Store.Admin().Get(ctx)
	if err != nil {
		return err // ErrNotFound is fine — bot has no admin yet
	}
	password, err := h.Cipher.Decrypt(admin.S21CredsEncrypted)
	if err != nil {
		_, _ = h.M.SendMessage(ctx, admin.TelegramID,
			"Internal: I can't decrypt your stored S21 credentials. Please re-run /admin.")
		return fmt.Errorf("decrypt admin creds for tid=%d: %w", admin.TelegramID, err)
	}
	_, authErr := h.S21.Authenticate(ctx, admin.S21Login, password)
	switch {
	case authErr == nil:
		return h.clearCredsFailure(ctx, admin)
	case errors.Is(authErr, s21.ErrInvalidCredentials):
		return h.handleCredsRejection(ctx, admin)
	default:
		// Transport / S21 outage. Don't change state, don't warn.
		return authErr
	}
}

// clearCredsFailure resets the failure markers when S21 accepts the stored
// creds again (after a temporary mismatch). Idempotent — no DB write when
// already healthy.
func (h *Handlers) clearCredsFailure(ctx context.Context, admin store.BotAdmin) error {
	if admin.S21CredsFailedAt == nil && admin.S21CredsLastWarnedAt == nil {
		return nil
	}
	admin.S21CredsFailedAt = nil
	admin.S21CredsLastWarnedAt = nil
	admin.UpdatedAt = h.Cfg.Now().UTC()
	if err := h.Store.Admin().Set(ctx, admin); err != nil {
		return fmt.Errorf("clear creds-failure markers: %w", err)
	}
	return nil
}

// handleCredsRejection runs when S21 returned ErrInvalidCredentials. It
// decides whether to send a warning DM (based on milestones), or to
// auto-unadmin (if 7d have elapsed since the first failure in this run).
func (h *Handlers) handleCredsRejection(ctx context.Context, admin store.BotAdmin) error {
	now := h.Cfg.Now().UTC()

	// Brand-new failure run.
	if admin.S21CredsFailedAt == nil {
		admin.S21CredsFailedAt = &now
		warned := now
		admin.S21CredsLastWarnedAt = &warned
		admin.UpdatedAt = now
		if err := h.Store.Admin().Set(ctx, admin); err != nil {
			return err
		}
		h.sendCredsWarning(ctx, admin.TelegramID, admin.S21Login, 0)
		return nil
	}

	elapsed := now.Sub(*admin.S21CredsFailedAt)

	// 7-day deadline reached: drop the admin row, send the final DM.
	if elapsed >= autoUnadminAfter {
		if err := h.Store.Admin().Delete(ctx); err != nil {
			return fmt.Errorf("auto-unadmin: delete row: %w", err)
		}
		msg := fmt.Sprintf(
			"S21 has rejected your stored credentials for the last %s. You are no longer the identity-bot admin. "+
				"Cached nicknames continue to work; new /provide_nickname calls will fail until someone runs /admin. "+
				"Run /admin yourself to reclaim the slot once you have a working password.",
			formatDuration(elapsed))
		if _, err := h.M.SendMessage(ctx, admin.TelegramID, msg); err != nil {
			log.Printf("auto-unadmin DM tid=%d: %v", admin.TelegramID, err)
		}
		log.Printf("auto-unadmin: cleared admin tid=%d after %s of S21 rejection", admin.TelegramID, elapsed)
		return nil
	}

	// Mid-run: send a milestone warning if one is due.
	if !milestoneDue(admin, now) {
		return nil
	}
	warned := now
	admin.S21CredsLastWarnedAt = &warned
	admin.UpdatedAt = now
	if err := h.Store.Admin().Set(ctx, admin); err != nil {
		return err
	}
	h.sendCredsWarning(ctx, admin.TelegramID, admin.S21Login, elapsed)
	return nil
}

// milestoneDue reports whether a warning DM should be sent at this tick.
// True when the largest milestone the elapsed time has crossed is greater
// than the last milestone the warning was sent for. We compute "last
// warned milestone" from (last_warned_at - failed_at).
func milestoneDue(admin store.BotAdmin, now time.Time) bool {
	if admin.S21CredsFailedAt == nil || admin.S21CredsLastWarnedAt == nil {
		return true
	}
	elapsed := now.Sub(*admin.S21CredsFailedAt)
	sinceWarn := admin.S21CredsLastWarnedAt.Sub(*admin.S21CredsFailedAt)
	for _, m := range warningMilestones {
		if elapsed >= m && sinceWarn < m {
			return true
		}
	}
	return false
}

// sendCredsWarning DMs the admin. `elapsed` is the time since the first
// failure (zero for the very first DM).
func (h *Handlers) sendCredsWarning(ctx context.Context, telegramID int64, login string, elapsed time.Duration) {
	var msg string
	if elapsed == 0 {
		msg = fmt.Sprintf(
			"Heads up: S21 just rejected your stored credentials for login %q. "+
				"If your password changed, re-run /admin to update. "+
				"I'll DM you again at 1d / 3d / 6d if it stays broken; at 7d I'll auto-step-you-down.",
			login)
	} else {
		msg = fmt.Sprintf(
			"S21 has rejected your stored credentials for %s now (login %q). "+
				"Please re-run /admin or you'll be auto-unadmined at the 7-day mark.",
			formatDuration(elapsed), login)
	}
	if _, err := h.M.SendMessage(ctx, telegramID, msg); err != nil {
		log.Printf("notify stale-creds admin tid=%d: %v", telegramID, err)
	}
}

// formatDuration renders an elapsed time as Nd Hh (e.g. "3d 4h"), trimmed
// for readability in DMs.
func formatDuration(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	default:
		return fmt.Sprintf("%dh", hours)
	}
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
