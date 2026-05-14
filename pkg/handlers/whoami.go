package handlers

import (
	"context"
	"errors"

	s21account "github.com/arseniisemenow/s21-account-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
)

// handleWhoami renders the caller's account row (login, campus, last-used,
// health). Returns "you're not logged in" if no row exists.
func (h *Handlers) handleWhoami(ctx context.Context, m *messenger.Message) error {
	a, err := h.Store.S21Accounts().Get(ctx, m.From.ID)
	if errors.Is(err, s21account.ErrNotFound) {
		return h.reply(ctx, m, "You're not logged in. Run /login to register your S21 credentials.")
	}
	if err != nil {
		return h.reply(ctx, m, "Couldn't read your account: "+err.Error())
	}
	return h.reply(ctx, m, s21account.RenderWhoami(a, h.Cfg.Now()))
}
