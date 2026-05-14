package handlers

import (
	"context"
	"regexp"
	"strings"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
)

// unadminPromptRegex matches the machine-readable header on the bot's
// confirmation prompt. The bot fully controls the prompt text, so a strict
// regex is safe. Mirrors the [KEY_OP=...] pattern used by /new_read_key.
var unadminPromptRegex = regexp.MustCompile(`^\[ADMIN_OP=unadmin\]`)

// handleUnadmin handles /unadmin. Two gates:
//
//  1. The bot must have an admin set. Otherwise there's nothing to do.
//  2. The caller's telegram_id must match the stored admin's telegram_id.
//     Anyone else gets a clear "you're not the admin" reply — no S21
//     prompt, no leaking who the admin is.
//
// On success the bot sends a force-reply confirmation prompt; the user
// replies "confirm" (any other text cancels), and handleUnadminReply
// completes the flow.
func (h *Handlers) handleUnadmin(ctx context.Context, m *messenger.Message) error {
	admin, err := h.Store.Admin().Get(ctx)
	if err != nil {
		return h.reply(ctx, m, "No admin is set right now — nothing to step down from.")
	}
	if admin.TelegramID != m.From.ID {
		return h.reply(ctx, m, "Only the current admin can step down. You aren't the admin.")
	}
	prompt := "[ADMIN_OP=unadmin]\n\n" +
		"You are about to step down as identity-bot admin.\n\n" +
		"After this:\n" +
		"- /provide_nickname and /my_nickname will fail until another admin runs /admin.\n" +
		"- Read keys you minted via /new_read_key STAY VALID (they're independent).\n\n" +
		"Reply with `confirm` to proceed. Any other reply cancels."
	if _, err := h.M.SendMessageWithForceReply(ctx, m.Chat.ID, prompt, "confirm"); err != nil {
		return h.reply(ctx, m, "Couldn't send confirmation prompt: "+err.Error())
	}
	return nil
}

// isUnadminReply mirrors isKeyFlowReply but matches the unadmin prompt.
func isUnadminReply(m *messenger.Message) bool {
	if m == nil || m.ReplyTo == nil || m.ReplyTo.From == nil || !m.ReplyTo.From.IsBot {
		return false
	}
	return unadminPromptRegex.MatchString(m.ReplyTo.Text)
}

// handleUnadminReply parses the confirmation reply and either deletes the
// admin row or replies "cancelled". The same telegram-identity check from
// handleUnadmin runs again — defence in depth in case someone else picked
// up the prompt and replied.
func (h *Handlers) handleUnadminReply(ctx context.Context, m *messenger.Message) error {
	admin, err := h.Store.Admin().Get(ctx)
	if err != nil {
		return h.reply(ctx, m, "No admin is set right now — nothing to do.")
	}
	if admin.TelegramID != m.From.ID {
		return h.reply(ctx, m, "Only the current admin can confirm this.")
	}
	if strings.TrimSpace(strings.ToLower(m.Text)) != "confirm" {
		return h.reply(ctx, m, "Cancelled — you are still the admin.")
	}
	if err := h.Store.Admin().Delete(ctx); err != nil {
		return h.reply(ctx, m, "Couldn't clear admin row: "+err.Error())
	}
	return h.reply(ctx, m,
		"You are no longer the identity-bot admin. /provide_nickname and /my_nickname will fail until somebody runs /admin again.")
}
