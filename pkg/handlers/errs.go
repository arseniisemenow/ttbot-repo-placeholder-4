package handlers

import (
	"context"
	"log"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
)

// userFacingError replies with a stable, user-readable message and logs
// the raw error server-side. Use this everywhere we used to surface
// err.Error() to the user — the raw text often carries internals
// (Telegram error bodies, YDB stack-ish strings) that confuse end-users
// and tell an attacker more than they should know.
//
// `where` is a short tag like "/login: send prompt" that lands in the
// log line so an operator can grep for the exact call site after a user
// reports an issue.
func (h *Handlers) userFacingError(ctx context.Context, m *messenger.Message, where, userMsg string, cause error) error {
	if cause != nil {
		log.Printf("user-facing error [%s] chat=%d user=%d: %v", where, m.Chat.ID, fromID(m), cause)
	}
	return h.reply(ctx, m, userMsg)
}

func fromID(m *messenger.Message) int64 {
	if m == nil || m.From == nil {
		return 0
	}
	return m.From.ID
}
