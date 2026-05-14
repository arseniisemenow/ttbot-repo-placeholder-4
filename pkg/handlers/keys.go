package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	s21account "github.com/arseniisemenow/s21-account-go"
	identityclient "github.com/arseniisemenow/s21-identity-client-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
	"github.com/arseniisemenow/s21-identity-bot/pkg/s21"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store"
)

// pendingDeleteDelay is how long after issuance the bot's plaintext-key DM
// is kept visible before the cron sweep removes it. Long enough for the user
// to copy comfortably; short enough that a forgotten DM doesn't linger.
const pendingDeleteDelay = 15 * time.Minute

// validKeyName matches the same character set the identity-service enforces
// on /admin/keys. Names are user-supplied so we validate locally too —
// surfacing a clean "use this character set" message instead of a 400 from
// the service.
var validKeyName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// promptHeaderRegex matches the machine-readable header line at the top of
// every key-flow prompt. The header carries the operation (new|revoke) and
// the key name so a force-reply round-trip stays stateless.
var promptHeaderRegex = regexp.MustCompile(`^\[KEY_OP=(new|revoke) name=([A-Za-z0-9_.-]+)\]`)

// handleNewReadKey kicks off the two-step /new_read_key flow. Step 1 here:
// validate the name, then send a force-reply prompt asking for S21 creds.
// Step 2 lives in handleKeyReply, dispatched when the user's reply lands.
func (h *Handlers) handleNewReadKey(ctx context.Context, m *messenger.Message, args string) error {
	name := strings.TrimSpace(args)
	if !validKeyName.MatchString(name) {
		return h.reply(ctx, m,
			"Usage: /new_read_key <name>\n\nName must be 1–64 chars: letters, digits, underscore, dot, dash.")
	}
	prompt := fmt.Sprintf(
		"[KEY_OP=new name=%s]\n\nReply with your S21 credentials as `login:password` (single message). I'll validate them, delete your reply immediately, and DM you the key. You can have only ONE active read key — revoke first if you already have one.",
		name)
	if _, err := h.M.SendMessageWithForceReply(ctx, m.Chat.ID, prompt, "login:password"); err != nil {
		return h.reply(ctx, m, "Couldn't send prompt: "+err.Error())
	}
	return nil
}

// handleRevokeReadKey kicks off the /revoke_read_key flow. Same shape as new.
func (h *Handlers) handleRevokeReadKey(ctx context.Context, m *messenger.Message, args string) error {
	name := strings.TrimSpace(args)
	if !validKeyName.MatchString(name) {
		return h.reply(ctx, m, "Usage: /revoke_read_key <name>")
	}
	prompt := fmt.Sprintf(
		"[KEY_OP=revoke name=%s]\n\nReply with your S21 credentials as `login:password` to revoke this key. Reply will be deleted after validation.",
		name)
	if _, err := h.M.SendMessageWithForceReply(ctx, m.Chat.ID, prompt, "login:password"); err != nil {
		return h.reply(ctx, m, "Couldn't send prompt: "+err.Error())
	}
	return nil
}

// isKeyFlowReply reports whether an inbound message looks like the user's
// credentials reply to one of our force-reply prompts. The decision is purely
// structural: it's a reply, the original is from a bot, and the original's
// first line carries the [KEY_OP=...] header we wrote.
func isKeyFlowReply(m *messenger.Message) bool {
	if m == nil || m.ReplyTo == nil || m.ReplyTo.From == nil || !m.ReplyTo.From.IsBot {
		return false
	}
	return promptHeaderRegex.MatchString(m.ReplyTo.Text)
}

// handleKeyReply parses the credentials reply, validates against S21,
// performs the mint or revoke, and finishes the round trip. The user's
// creds message gets deleted immediately after we've read the text.
func (h *Handlers) handleKeyReply(ctx context.Context, m *messenger.Message) error {
	matches := promptHeaderRegex.FindStringSubmatch(m.ReplyTo.Text)
	if matches == nil {
		return nil
	}
	op, name := matches[1], matches[2]

	// Always attempt to scrub the user's creds message, even on failure.
	// Best-effort: a Telegram outage shouldn't prevent the rest of the flow,
	// just log.
	defer func() {
		if err := h.M.DeleteMessage(ctx, m.Chat.ID, m.MessageID); err != nil {
			log.Printf("delete creds message chat=%d msg=%d: %v", m.Chat.ID, m.MessageID, err)
		}
	}()

	login, password, ok := parseLoginPassword(strings.TrimSpace(m.Text))
	if !ok {
		return h.reply(ctx, m, "Couldn't read creds — expected `login:password` in a single line. Try /"+op+"_read_key "+name+" again.")
	}
	// Validate fresh against S21 — we never store these creds. If it's a
	// bad-credentials error, give the user a precise message; on transport
	// errors, ask them to retry.
	profile, err := h.S21.Authenticate(ctx, login, password)
	switch {
	case errors.Is(err, s21.ErrInvalidCredentials):
		return h.reply(ctx, m, "S21 rejected those credentials. Try again with the correct login and password.")
	case err != nil:
		return h.reply(ctx, m, "S21 is unavailable right now. Try again shortly.")
	}

	switch op {
	case "new":
		return h.completeNewKey(ctx, m, name, login, password, profile)
	case "revoke":
		return h.completeRevokeKey(ctx, m, name, login, password)
	}
	return nil
}

// completeNewKey calls identity-service to mint, posts the plaintext key
// back to the user, and schedules that DM for deferred deletion.
func (h *Handlers) completeNewKey(ctx context.Context, m *messenger.Message, name, login, password string, profile s21.Profile) error {
	cli, err := h.identityServiceClient(login, password)
	if err != nil {
		return h.reply(ctx, m, err.Error())
	}
	resp, err := cli.CreateAPIKey(ctx, identityclient.CreateAPIKeyRequest{
		Name:                name,
		Scopes:              "read",
		CreatedByTelegramID: m.From.ID,
	})
	if err != nil {
		switch {
		case errors.Is(err, identityclient.ErrConflict):
			return h.reply(ctx, m,
				"You already have an active key, or the name is taken. Run /revoke_read_key <name> first, or pick a different name.")
		case errors.Is(err, identityclient.ErrBadRequest):
			return h.reply(ctx, m, "Bad request: "+err.Error())
		case errors.Is(err, identityclient.ErrInvalidS21Token):
			return h.reply(ctx, m, "Identity service rejected the S21 credentials.")
		case errors.Is(err, identityclient.ErrUnavailable):
			return h.reply(ctx, m, "Identity service unavailable. Try again shortly.")
		}
		return h.reply(ctx, m, "Mint failed: "+err.Error())
	}
	keyMsg := fmt.Sprintf(
		"KEY CREATED — copy this NOW. It will vanish from this chat in about %d minutes and is never recoverable.\n\nname:   %s\nscopes: %s\nkey:    %s\n\nAuthenticated as %s (%s).",
		int(pendingDeleteDelay.Minutes()), resp.Name, resp.Scopes, resp.Key, profile.Login, profile.CampusName,
	)
	msgID, err := h.M.SendMessage(ctx, m.Chat.ID, keyMsg)
	if err != nil {
		// We minted the key but couldn't deliver it. Tell the user — they
		// can revoke and try again. (Don't auto-revoke: a stuck DM doesn't
		// mean the key didn't propagate.)
		return h.reply(ctx, m, "Key was minted but I couldn't DM it to you. Run /revoke_read_key "+name+" and try again.")
	}
	due := h.Cfg.Now().UTC().Add(pendingDeleteDelay)
	if err := h.Store.PendingDeletes().Insert(ctx, store.PendingDelete{
		ChatID:    m.Chat.ID,
		MessageID: msgID,
		DeleteAt:  due,
	}); err != nil {
		log.Printf("schedule key-DM delete chat=%d msg=%d: %v", m.Chat.ID, msgID, err)
	}
	return nil
}

func (h *Handlers) completeRevokeKey(ctx context.Context, m *messenger.Message, name, login, password string) error {
	cli, err := h.identityServiceClient(login, password)
	if err != nil {
		return h.reply(ctx, m, err.Error())
	}
	if err := cli.RevokeAPIKey(ctx, name, m.From.ID); err != nil {
		switch {
		case errors.Is(err, identityclient.ErrNotFound):
			return h.reply(ctx, m, "No active key named "+name+" registered to you.")
		case errors.Is(err, identityclient.ErrInvalidS21Token):
			return h.reply(ctx, m, "Identity service rejected the S21 credentials.")
		case errors.Is(err, identityclient.ErrUnavailable):
			return h.reply(ctx, m, "Identity service unavailable. Try again shortly.")
		}
		return h.reply(ctx, m, "Revoke failed: "+err.Error())
	}
	return h.reply(ctx, m, "Revoked "+name+".")
}

// identityServiceClient builds an identityclient using fresh user-supplied
// S21 creds (NOT the stored bot-admin creds), and the bot's own write-scope
// API key for the perimeter check.
func (h *Handlers) identityServiceClient(login, password string) (*identityclient.Client, error) {
	if h.Cfg.IdentityServiceAPIKey == "" {
		return nil, errors.New("identity bot is missing its IDENTITY_SERVICE_API_KEY env — operator must mint it via the CLI and redeploy")
	}
	return identityclient.New(
		h.Cfg.IdentityBaseURL, login, password,
		identityclient.WithAPIKey(h.Cfg.IdentityServiceAPIKey),
	), nil
}

// handleMyKeys lists the caller's read keys by name and status. No S21
// prompt — we route through PickHealthy to grab whichever logged-in row
// has working creds, then filter the listing to the caller's telegram_id.
// Useful when a user forgot what they named their key and wants to revoke it.
func (h *Handlers) handleMyKeys(ctx context.Context, m *messenger.Message) error {
	var keys []identityclient.APIKeyInfo
	err := h.withIdentityClient(ctx, func(cli *identityclient.Client) error {
		got, err := cli.ListAPIKeys(ctx)
		if err != nil {
			return err
		}
		keys = got
		return nil
	})
	if errors.Is(err, s21account.ErrNoHealthy) {
		return h.reply(ctx, m, "No healthy S21 accounts available right now — somebody needs to /login.")
	}
	if err != nil {
		return h.reply(ctx, m, "Couldn't list keys: "+err.Error())
	}
	var mine []identityclient.APIKeyInfo
	for _, k := range keys {
		if k.CreatedByTelegramID == m.From.ID {
			mine = append(mine, k)
		}
	}
	if len(mine) == 0 {
		return h.reply(ctx, m, "You have no read keys. Mint one with /new_read_key <name>.")
	}
	var b strings.Builder
	b.WriteString("Your read keys:\n")
	for _, k := range mine {
		status := "active"
		if k.RevokedAt != nil {
			status = "revoked " + k.RevokedAt.UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "- %s (%s, %s)\n", k.Name, k.Scopes, status)
	}
	b.WriteString("\nRevoke with /revoke_read_key <name>.")
	return h.reply(ctx, m, b.String())
}
