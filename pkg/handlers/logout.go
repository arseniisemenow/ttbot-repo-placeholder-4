package handlers

import (
	"context"
	"errors"
	"regexp"
	"strings"

	s21account "github.com/arseniisemenow/s21-account-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
)

// logoutPromptRegex matches the bot's /logout confirmation prompt header.
var logoutPromptRegex = regexp.MustCompile(`^\[LOGIN_OP=logout\]`)

// handleLogout starts the two-step /logout flow. The caller must be
// logged in; only then do we send the confirm prompt.
func (h *Handlers) handleLogout(ctx context.Context, m *messenger.Message) error {
	if _, err := h.Store.S21Accounts().Get(ctx, m.From.ID); errors.Is(err, s21account.ErrNotFound) {
		return h.reply(ctx, m, "You're not logged in — nothing to log out from.")
	}
	prompt := "[LOGIN_OP=logout]\n\n" +
		"You are about to log out (your stored S21 creds for this bot will be deleted).\n\n" +
		"After this:\n" +
		"- Other logged-in accounts continue to back the bot's S21 calls; only your row is removed.\n" +
		"- Read keys you minted via /new_read_key STAY VALID.\n\n" +
		"Reply with `confirm` to proceed. Any other reply cancels."
	if _, err := h.M.SendMessageWithForceReply(ctx, m.Chat.ID, prompt, "confirm"); err != nil {
		return h.reply(ctx, m, "Couldn't send confirmation prompt: "+err.Error())
	}
	return nil
}

// isLogoutReply detects the confirm reply.
func isLogoutReply(m *messenger.Message) bool {
	if m == nil || m.ReplyTo == nil || m.ReplyTo.From == nil || !m.ReplyTo.From.IsBot {
		return false
	}
	return logoutPromptRegex.MatchString(m.ReplyTo.Text)
}

// handleLogoutReply parses the confirm reply. Anything other than "confirm"
// cancels.
func (h *Handlers) handleLogoutReply(ctx context.Context, m *messenger.Message) error {
	if strings.TrimSpace(strings.ToLower(m.Text)) != "confirm" {
		return h.reply(ctx, m, "Cancelled — you are still logged in.")
	}
	if err := h.Store.S21Accounts().Delete(ctx, m.From.ID); err != nil {
		return h.reply(ctx, m, "Couldn't delete your account row: "+err.Error())
	}
	return h.reply(ctx, m, "Logged out. Your stored S21 credentials have been removed. /login again whenever you want.")
}
