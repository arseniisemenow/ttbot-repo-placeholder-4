package handlers

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/messenger"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/s21"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store"
)

// adminSetPromptRegex matches the bot's two-step /admin prompt header.
// Mirrors the [KEY_OP=...] / [ADMIN_OP=unadmin] machine-readable tags so the
// reply detector can route stateless replies.
var adminSetPromptRegex = regexp.MustCompile(`^\[ADMIN_OP=set\]`)

// isAdminSetReply reports whether an inbound DM looks like the user's
// credentials reply to the /admin force-reply prompt.
func isAdminSetReply(m *messenger.Message) bool {
	if m == nil || m.ReplyTo == nil || m.ReplyTo.From == nil || !m.ReplyTo.From.IsBot {
		return false
	}
	return adminSetPromptRegex.MatchString(m.ReplyTo.Text)
}

// handleAdminSetReply completes the /admin flow. The user's reply contains
// `login:password`. We delete the message immediately (creds are sensitive),
// re-check the first-wins-by-identity gate, validate against S21, and store
// the encrypted row.
func (h *Handlers) handleAdminSetReply(ctx context.Context, m *messenger.Message) error {
	// Best-effort scrub of the user's message — runs even if validation
	// below fails so the creds don't linger in chat history.
	defer func() {
		if err := h.M.DeleteMessage(ctx, m.Chat.ID, m.MessageID); err != nil {
			log.Printf("delete /admin creds message chat=%d msg=%d: %v", m.Chat.ID, m.MessageID, err)
		}
	}()

	// First-wins by identity (defense in depth: same check ran at /admin
	// time, but we re-check here in case state changed in between).
	if existing, err := h.Store.Admin().Get(ctx); err == nil && existing.TelegramID != m.From.ID {
		return h.reply(ctx, m,
			"This bot already has an admin and only they can rotate the credentials.")
	}

	login, password, ok := parseLoginPassword(strings.TrimSpace(m.Text))
	if !ok {
		return h.reply(ctx, m,
			"Couldn't read creds — expected `login:password` on a single line. Run /admin again to start over.")
	}
	profile, err := h.S21.Authenticate(ctx, login, password)
	switch {
	case errors.Is(err, s21.ErrInvalidCredentials):
		return h.reply(ctx, m, "S21 rejected those credentials. Run /admin again to retry.")
	case err != nil:
		return h.reply(ctx, m, "S21 is unavailable right now. Try again later.")
	}
	enc, err := h.Cipher.Encrypt(password)
	if err != nil {
		return err
	}
	row := store.BotAdmin{
		TelegramID:        m.From.ID,
		S21Login:          login,
		S21CredsEncrypted: enc,
		UpdatedAt:         h.Cfg.Now().UTC(),
	}
	if err := h.Store.Admin().Set(ctx, row); err != nil {
		return err
	}
	return h.reply(ctx, m,
		"You are now the identity-bot admin ("+profile.Login+", "+profile.CampusName+"). The bot will use your S21 credentials to validate user nicknames.")
}
