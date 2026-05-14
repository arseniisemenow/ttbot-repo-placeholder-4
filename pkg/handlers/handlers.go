// Package handlers wires Telegram updates to identity-service calls and the
// local bot_admin row.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	identityclient "github.com/arseniisemenow/ttbot-repo-placeholder-2"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/crypto"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/messenger"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/s21"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store"
)

// Config carries injectable settings.
type Config struct {
	IdentityBaseURL string // base URL of placeholder-3 identity service
	// IdentityServiceAPIKey is the bot's own write-scope X-Api-Key, used to
	// call /admin/keys for read-key minting/revocation. Created via the
	// admin CLI once at bootstrap, then pasted into the bot's env.
	IdentityServiceAPIKey string
	Now                   func() time.Time // injectable clock
}

// Handlers is the dependency bag for command routing.
type Handlers struct {
	Store  store.Store
	M      messenger.Messenger
	S21    s21.Client
	Cipher *crypto.Cipher
	Cfg    Config
}

// New constructs Handlers.
func New(st store.Store, m messenger.Messenger, s21c s21.Client, cipher *crypto.Cipher, cfg Config) *Handlers {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Handlers{Store: st, M: m, S21: s21c, Cipher: cipher, Cfg: cfg}
}

// Dispatch routes one update through the command tree.
func (h *Handlers) Dispatch(ctx context.Context, u *messenger.Update) error {
	if u == nil || u.Message == nil || u.Message.From == nil {
		return nil
	}
	m := u.Message
	if m.Chat.Type != "private" {
		// Identity bot only operates in DMs.
		return nil
	}
	// Credentials-reply detection runs BEFORE the command switch: a /key-flow
	// reply has text that looks like "login:password" with no leading slash,
	// so it'd otherwise fall through. The detection is structural — must be
	// a reply to a bot message whose first line carries our [KEY_OP=...]
	// header.
	if isKeyFlowReply(m) {
		return h.handleKeyReply(ctx, m)
	}
	if isUnadminReply(m) {
		return h.handleUnadminReply(ctx, m)
	}
	if isAdminSetReply(m) {
		return h.handleAdminSetReply(ctx, m)
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return nil
	}
	cmd, args := splitCommand(text)
	switch cmd {
	case "/start", "/help":
		return h.handleStart(ctx, m)
	case "/admin":
		return h.handleAdmin(ctx, m, args)
	case "/provide_nickname":
		return h.handleProvideNickname(ctx, m, args)
	case "/remove_nickname":
		return h.handleRemoveNickname(ctx, m)
	case "/my_nickname":
		return h.handleMyNickname(ctx, m)
	case "/list_users":
		return h.handleListUsers(ctx, m)
	case "/new_read_key":
		return h.handleNewReadKey(ctx, m, args)
	case "/revoke_read_key":
		return h.handleRevokeReadKey(ctx, m, args)
	case "/my_keys":
		return h.handleMyKeys(ctx, m)
	case "/unadmin":
		return h.handleUnadmin(ctx, m)
	}
	return nil
}

func splitCommand(text string) (string, string) {
	parts := strings.SplitN(text, " ", 2)
	cmd := parts[0]
	if at := strings.Index(cmd, "@"); at >= 0 {
		cmd = cmd[:at]
	}
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return cmd, args
}

func (h *Handlers) reply(ctx context.Context, m *messenger.Message, text string) error {
	_, err := h.M.SendMessage(ctx, m.Chat.ID, text)
	if err != nil {
		log.Printf("reply: %v", err)
	}
	return err
}

// ---------------- commands ----------------

func (h *Handlers) handleStart(ctx context.Context, m *messenger.Message) error {
	return h.reply(ctx, m,
		"Hi! I'm the S21 identity bot.\n\n"+
			"Commands:\n"+
			"/provide_nickname <s21_login> — register your S21 nickname for this Telegram account.\n"+
			"/remove_nickname — clear your registered nickname.\n"+
			"/my_nickname — show what's stored for you.\n\n"+
			"API access (S21 admins):\n"+
			"/new_read_key <name> — mint a read-only identity-service API key. Two-step: I'll prompt for your S21 creds in a reply.\n"+
			"/revoke_read_key <name> — revoke a read key you created. Same two-step flow.\n"+
			"/my_keys — list the keys you've created (names + status). No S21 prompt.\n\n"+
			"Administrators:\n"+
			"/admin — claim the admin role. Two-step: I'll prompt for your S21 creds in a reply, validate against S21, and delete your reply immediately. First-wins by identity: only the original admin can rotate later.\n"+
			"/unadmin — step down as admin (two-step confirm). Only the current admin can run this.\n"+
			"/list_users — list every registered user.")
}

// handleAdmin is the first half of the two-step admin flow:
//
//  1. /admin (no args) — send a force-reply prompt asking for `login:password`.
//  2. handleAdminSetReply (admin_set.go) — read the reply, scrub the message,
//     validate against S21, store the encrypted row.
//
// First-wins by identity: only the original admin's telegram_id can re-run
// /admin to rotate the row. A different telegram_id gets rejected here so we
// never even prompt them. The same check repeats in step 2 for defence.
//
// Inline-args form (`/admin login:password`) is rejected — Telegram keeps
// command text in chat history so creds in the command line are exposed.
func (h *Handlers) handleAdmin(ctx context.Context, m *messenger.Message, args string) error {
	if strings.TrimSpace(args) != "" {
		return h.reply(ctx, m,
			"Inline credentials are no longer accepted because Telegram keeps them in chat history. "+
				"Run /admin with no arguments — I'll prompt you to reply with your S21 creds and delete that reply immediately.")
	}
	if existing, err := h.Store.Admin().Get(ctx); err == nil && existing.TelegramID != m.From.ID {
		return h.reply(ctx, m,
			"This bot already has an admin and only they can rotate the credentials. "+
				"If you ARE the admin, message me from your registered Telegram account.")
	}
	prompt := "[ADMIN_OP=set]\n\n" +
		"Reply with your S21 credentials as `login:password` on a single line. " +
		"I'll authenticate against S21, encrypt the result, and **delete your reply immediately** so the creds don't linger in this chat."
	if _, err := h.M.SendMessageWithForceReply(ctx, m.Chat.ID, prompt, "login:password"); err != nil {
		return h.reply(ctx, m, "Couldn't send the prompt: "+err.Error())
	}
	return nil
}

func (h *Handlers) handleProvideNickname(ctx context.Context, m *messenger.Message, args string) error {
	nick := strings.TrimSpace(args)
	if nick == "" {
		return h.reply(ctx, m, "Usage: /provide_nickname <s21_login>")
	}
	cli, err := h.identityClient(ctx)
	if err != nil {
		return h.reply(ctx, m, err.Error())
	}
	if _, err := cli.PutUser(ctx, m.From.ID, nick); err != nil {
		return h.reply(ctx, m, identityErrorMessage(err))
	}
	return h.reply(ctx, m, "Nickname registered: "+nick+".")
}

func (h *Handlers) handleRemoveNickname(ctx context.Context, m *messenger.Message) error {
	cli, err := h.identityClient(ctx)
	if err != nil {
		return h.reply(ctx, m, err.Error())
	}
	if err := cli.DeleteUser(ctx, m.From.ID); err != nil {
		return h.reply(ctx, m, identityErrorMessage(err))
	}
	return h.reply(ctx, m, "Nickname cleared.")
}

func (h *Handlers) handleMyNickname(ctx context.Context, m *messenger.Message) error {
	cli, err := h.identityClient(ctx)
	if err != nil {
		return h.reply(ctx, m, err.Error())
	}
	u, err := cli.GetUserByTelegram(ctx, m.From.ID)
	switch {
	case errors.Is(err, identityclient.ErrNotFound):
		return h.reply(ctx, m, "You don't have a nickname registered. Run /provide_nickname <s21_login>.")
	case err != nil:
		return h.reply(ctx, m, identityErrorMessage(err))
	}
	return h.reply(ctx, m, fmt.Sprintf(
		"Your nickname: %s\nCampus: %s\nCoalition: %s",
		u.Nickname, u.CampusName, defaultIfEmpty(u.CoalitionName, "—"),
	))
}

func (h *Handlers) handleListUsers(ctx context.Context, m *messenger.Message) error {
	admin, err := h.Store.Admin().Get(ctx)
	if err != nil || admin.TelegramID != m.From.ID {
		return h.reply(ctx, m, "Only the identity-bot admin can run /list_users.")
	}
	// /list_users on the service side doesn't exist — by design, the service
	// only resolves by telegram_id or nickname. For an admin-level listing,
	// the future approach is a `/admin/users` endpoint. For now we explain
	// the limitation rather than fake it.
	return h.reply(ctx, m, "Listing all users is not yet implemented — the service exposes lookups by telegram_id and by nickname only. Use the service URL directly if you need a full dump.")
}

// ---------------- helpers ----------------

func (h *Handlers) identityClient(ctx context.Context) (*identityclient.Client, error) {
	admin, err := h.Store.Admin().Get(ctx)
	if err != nil {
		return nil, errors.New("Identity bot has no admin set yet. Ask the operator to run /admin <login>:<password>.")
	}
	password, err := h.Cipher.Decrypt(admin.S21CredsEncrypted)
	if err != nil {
		return nil, errors.New("Internal error decrypting admin credentials. Operator must re-run /admin.")
	}
	opts := []identityclient.Option{}
	if h.Cfg.IdentityServiceAPIKey != "" {
		opts = append(opts, identityclient.WithAPIKey(h.Cfg.IdentityServiceAPIKey))
	}
	return identityclient.New(h.Cfg.IdentityBaseURL, admin.S21Login, password, opts...), nil
}

func parseLoginPassword(s string) (login, password string, ok bool) {
	idx := strings.IndexByte(s, ':')
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

func identityErrorMessage(err error) string {
	switch {
	case errors.Is(err, identityclient.ErrInvalidS21Token):
		return "Identity service rejected the admin's S21 credentials. Operator must re-run /admin."
	case errors.Is(err, identityclient.ErrNotFound):
		return "Not found."
	case errors.Is(err, identityclient.ErrConflict):
		return "Conflict with existing state."
	case errors.Is(err, identityclient.ErrBadRequest):
		return "Bad request — likely the S21 nickname doesn't exist."
	case errors.Is(err, identityclient.ErrUnavailable):
		return "Identity service unavailable; try again shortly."
	default:
		return "Error: " + err.Error()
	}
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// silence unused imports if a refactor leaves them dangling.
var _ = sort.Slice
var _ = strconv.Itoa
