package handlers

import (
	"context"
	"errors"
	"fmt"

	s21account "github.com/arseniisemenow/s21-account-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
)

// handleWhoami renders the caller's S21 account state plus a count of
// how many other accounts are currently logged in.
func (h *Handlers) handleWhoami(ctx context.Context, m *messenger.Message) error {
	a, err := h.Store.S21Accounts().Get(ctx, m.From.ID)
	if errors.Is(err, s21account.ErrNotFound) {
		return h.reply(ctx, m, "You're not logged in. Run /login to register your S21 credentials.")
	}
	if err != nil {
		return h.userFacingError(ctx, m, "/whoami: read account",
			"The database is unreachable right now — try again shortly.", err)
	}
	body := s21account.RenderWhoami(a, h.Cfg.Now())
	if all, listErr := h.Store.S21Accounts().List(ctx); listErr == nil {
		others := 0
		for _, row := range all {
			if row.TelegramID != m.From.ID {
				others++
			}
		}
		body += "\n\n" + otherAccountsLine(others)
	}
	return h.reply(ctx, m, body)
}

// otherAccountsLine — small footer with grammar that's right for 0/1/N.
func otherAccountsLine(n int) string {
	switch n {
	case 0:
		return "You're the only logged-in account right now."
	case 1:
		return "Together with 1 other logged-in account."
	default:
		return fmt.Sprintf("Together with %d other logged-in accounts.", n)
	}
}
