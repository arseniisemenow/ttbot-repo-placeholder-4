// Package handlers wires Telegram updates to identity-service calls.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	s21account "github.com/arseniisemenow/s21-account-go"
	identityclient "github.com/arseniisemenow/s21-identity-client-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/crypto"
	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
	"github.com/arseniisemenow/s21-identity-bot/pkg/s21"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store"
)

// Config carries injectable settings.
type Config struct {
	IdentityBaseURL string // base URL of the identity service
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
	// Force-reply detectors run BEFORE the command switch — credential
	// replies have no leading slash so they'd otherwise fall through.
	if isKeyFlowReply(m) {
		return h.handleKeyReply(ctx, m)
	}
	if isLogoutReply(m) {
		return h.handleLogoutReply(ctx, m)
	}
	if isLoginReply(m) {
		return h.handleLoginReply(ctx, m)
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return nil
	}
	cmd, args := splitCommand(text)
	switch cmd {
	case "/start", "/help":
		return h.handleStart(ctx, m)
	case "/login":
		return h.handleLogin(ctx, m, args)
	case "/logout":
		return h.handleLogout(ctx, m)
	case "/whoami":
		return h.handleWhoami(ctx, m)
	case "/provide_nickname":
		return h.handleProvideNickname(ctx, m, args)
	case "/remove_nickname":
		return h.handleRemoveNickname(ctx, m)
	case "/my_nickname":
		return h.handleMyNickname(ctx, m)
	case "/new_read_key":
		return h.handleNewReadKey(ctx, m, args)
	case "/revoke_read_key":
		return h.handleRevokeReadKey(ctx, m, args)
	case "/my_keys":
		return h.handleMyKeys(ctx, m)
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
			"I keep a directory of School-21 nicknames so other bots (e.g. @s21_table_tennis_bot) "+
			"can find players by Telegram username. To register yourself, run "+
			"/provide_nickname <your_s21_nickname>.\n\n"+
			"Nickname registration:\n"+
			"/provide_nickname <s21_nickname> — register your School-21 nickname for this Telegram account.\n"+
			"/remove_nickname — clear your registered nickname.\n"+
			"/my_nickname — show what's stored for you.\n\n"+
			"S21 session (the bot uses your S21 creds to validate nicknames):\n"+
			"/login — store your S21 creds. Two-step: I'll prompt you to reply with `login:password`, validate against S21, and delete your reply immediately. "+
			"Multiple users can /login; the bot picks healthy stored credentials per call.\n"+
			"/logout — remove your stored S21 creds. Two-step confirm.\n"+
			"/whoami — show whether you're logged in and the health of your creds.\n\n"+
			"API keys:\n"+
			"/new_read_key <name> — mint a read-only identity-service API key. Two-step.\n"+
			"/revoke_read_key <name> — revoke a read key you created. Two-step.\n"+
			"/my_keys — list the keys you've created.")
}

func (h *Handlers) handleProvideNickname(ctx context.Context, m *messenger.Message, args string) error {
	nick := strings.TrimSpace(args)
	if nick == "" {
		return h.reply(ctx, m, "Usage: /provide_nickname <s21_nickname>")
	}
	err := h.withIdentityClient(ctx, func(cli *identityclient.Client) error {
		_, err := cli.PutUser(ctx, m.From.ID, nick)
		return err
	})
	if errors.Is(err, s21account.ErrNoHealthy) {
		return h.reply(ctx, m, "No healthy S21 accounts available right now — somebody needs to /login.")
	}
	if err != nil {
		return h.reply(ctx, m, identityErrorMessage(err))
	}
	return h.reply(ctx, m, "Nickname registered: "+nick+".")
}

func (h *Handlers) handleRemoveNickname(ctx context.Context, m *messenger.Message) error {
	err := h.withIdentityClient(ctx, func(cli *identityclient.Client) error {
		return cli.DeleteUser(ctx, m.From.ID)
	})
	if errors.Is(err, s21account.ErrNoHealthy) {
		return h.reply(ctx, m, "No healthy S21 accounts available right now — somebody needs to /login.")
	}
	if err != nil {
		return h.reply(ctx, m, identityErrorMessage(err))
	}
	return h.reply(ctx, m, "Nickname cleared.")
}

func (h *Handlers) handleMyNickname(ctx context.Context, m *messenger.Message) error {
	var u identityclient.User
	err := h.withIdentityClient(ctx, func(cli *identityclient.Client) error {
		got, err := cli.GetUserByTelegram(ctx, m.From.ID)
		if err != nil {
			return err
		}
		u = got
		return nil
	})
	switch {
	case errors.Is(err, s21account.ErrNoHealthy):
		return h.reply(ctx, m, "No healthy S21 accounts available right now — somebody needs to /login.")
	case errors.Is(err, identityclient.ErrNotFound):
		return h.reply(ctx, m, "You don't have a nickname registered. Run /provide_nickname <s21_nickname>.")
	case err != nil:
		return h.reply(ctx, m, identityErrorMessage(err))
	}
	return h.reply(ctx, m, fmt.Sprintf(
		"Your nickname: %s\nCampus: %s\nCoalition: %s",
		u.Nickname, u.CampusName, defaultIfEmpty(u.CoalitionName, "—"),
	))
}

// ---------------- helpers ----------------

// withIdentityClient runs `fn` against an identity-service client whose
// X-S21-Token is sourced from a healthy s21_accounts row. On
// identityclient.ErrInvalidS21Token from `fn`, marks that row bad (via the
// shared package's PickHealthy) and retries with the next healthy row.
func (h *Handlers) withIdentityClient(ctx context.Context, fn func(*identityclient.Client) error) error {
	return s21account.PickHealthy(ctx, h.Store.S21Accounts(), h.Cipher, h.Cfg.Now(),
		func(login, password string) error {
			opts := []identityclient.Option{}
			if h.Cfg.IdentityServiceAPIKey != "" {
				opts = append(opts, identityclient.WithAPIKey(h.Cfg.IdentityServiceAPIKey))
			}
			cli := identityclient.New(h.Cfg.IdentityBaseURL, login, password, opts...)
			err := fn(cli)
			if errors.Is(err, identityclient.ErrInvalidS21Token) {
				// Tell PickHealthy to mark this row bad and try the next.
				return s21account.ErrInvalidCredentials
			}
			return err
		})
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
		return "Identity service rejected the stored S21 credentials. Run /login with fresh creds."
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
