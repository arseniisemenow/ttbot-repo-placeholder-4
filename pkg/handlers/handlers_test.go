package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	s21account "github.com/arseniisemenow/s21-account-go"
	identityclient "github.com/arseniisemenow/s21-identity-client-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/crypto"
	"github.com/arseniisemenow/s21-identity-bot/pkg/handlers"
	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
	"github.com/arseniisemenow/s21-identity-bot/pkg/s21"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store/memstore"
)

// world is the test fixture: fresh memstore, mock messenger, mock S21, fake
// identity service via httptest, fully-wired Handlers.
//
// These tests cover only this bot's WIRING — command routing, two-step force-
// reply flows, periodic-job glue, and the identity-service round trip. The
// underlying credential lifecycle (mark bad → warn → 7d auto-logout) is owned
// by github.com/arseniisemenow/s21-account-go and tested there; we don't
// re-cover it here.
type world struct {
	t         *testing.T
	ctx       context.Context
	store     *memstore.Store
	M         *messenger.Mock
	S21       *s21.Mock
	cipher    *crypto.Cipher
	srv       *httptest.Server
	handlers  *handlers.Handlers
	idService *fakeIdentityService
	clock     time.Time
}

type fakeIdentityService struct {
	mu       sync.Mutex
	users    map[int64]identityclient.User
	failNext error
	calls    []string
}

func newFakeIdentityService() *fakeIdentityService {
	return &fakeIdentityService{users: map[int64]identityclient.User{}}
}

func (f *fakeIdentityService) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if f.failNext != nil {
			http.Error(w, f.failNext.Error(), http.StatusUnauthorized)
			f.failNext = nil
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/by_telegram/") && r.Method == http.MethodGet:
			tid := parseTID(r.URL.Path)
			u, ok := f.users[tid]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(u)
		case strings.HasPrefix(r.URL.Path, "/users/by_telegram/") && r.Method == http.MethodPut:
			tid := parseTID(r.URL.Path)
			var body struct {
				Nickname string `json:"nickname"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			u := identityclient.User{
				TelegramID: tid, Nickname: body.Nickname,
				CampusName: "21 Kazan", CampusID: "kazan",
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			f.users[tid] = u
			_ = json.NewEncoder(w).Encode(u)
		case strings.HasPrefix(r.URL.Path, "/users/by_telegram/") && r.Method == http.MethodDelete:
			tid := parseTID(r.URL.Path)
			if _, ok := f.users[tid]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(f.users, tid)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "route not in fake", http.StatusNotFound)
		}
	})
}

func parseTID(path string) int64 {
	tail := strings.TrimPrefix(path, "/users/by_telegram/")
	var v int64
	for _, c := range tail {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int64(c-'0')
	}
	return v
}

func newWorld(t *testing.T) *world {
	t.Helper()
	st := memstore.New()
	mes := messenger.NewMock()
	sm := s21.NewMock()
	cipher, err := crypto.NewFromKey(make32ByteKey())
	if err != nil {
		t.Fatal(err)
	}
	idSvc := newFakeIdentityService()
	ts := httptest.NewServer(idSvc.handler())
	t.Cleanup(ts.Close)
	w := &world{
		t:         t,
		ctx:       context.Background(),
		store:     st,
		M:         mes,
		S21:       sm,
		cipher:    cipher,
		srv:       ts,
		idService: idSvc,
		clock:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	w.handlers = handlers.New(st, mes, sm, cipher, handlers.Config{
		IdentityBaseURL: ts.URL,
		Now:             func() time.Time { return w.clock },
	})
	return w
}

func (w *world) advanceClock(d time.Duration) { w.clock = w.clock.Add(d) }

func (w *world) runCron() {
	w.t.Helper()
	if err := w.handlers.PeriodicJob(w.ctx); err != nil {
		w.t.Fatalf("cron: %v", err)
	}
}

func make32ByteKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// dm dispatches a private-chat text message as if it arrived from Telegram.
func (w *world) dm(from int64, username, text string) {
	w.t.Helper()
	upd := &messenger.Update{
		Message: &messenger.Message{
			MessageID: 1,
			Chat:      messenger.Chat{ID: from, Type: "private"},
			From:      &messenger.User{ID: from, Username: username},
			Text:      text,
		},
	}
	if err := w.handlers.Dispatch(w.ctx, upd); err != nil {
		w.t.Fatalf("dispatch: %v", err)
	}
}

// dmReply dispatches a reply to the bot's most recent force-reply prompt.
func (w *world) dmReply(from int64, username, text string) {
	w.t.Helper()
	last, ok := lastBotMessage(w.M.Calls)
	if !ok {
		w.t.Fatal("no prior bot message to reply to")
	}
	upd := &messenger.Update{
		Message: &messenger.Message{
			MessageID: 999,
			Chat:      messenger.Chat{ID: from, Type: "private"},
			From:      &messenger.User{ID: from, Username: username},
			Text:      text,
			ReplyTo: &messenger.Message{
				MessageID: 42,
				From:      &messenger.User{ID: 0, IsBot: true},
				Text:      last.Text,
			},
		},
	}
	if err := w.handlers.Dispatch(w.ctx, upd); err != nil {
		w.t.Fatalf("dispatch reply: %v", err)
	}
}

// loginViaCommand exercises the two-step /login flow end-to-end. Resets the
// recorded calls afterwards so subsequent assertions are clean.
func (w *world) loginViaCommand(tid int64, username, login, password string) {
	w.t.Helper()
	w.dm(tid, username, "/login")
	w.dmReply(tid, username, login+":"+password)
	w.M.Calls = nil
}

func (w *world) assertReplyContains(substr string) {
	w.t.Helper()
	for _, c := range w.M.Calls {
		if strings.Contains(c.Text, substr) {
			return
		}
	}
	w.t.Fatalf("no reply contains %q; got %d replies (%s)", substr, len(w.M.Calls), summarize(w.M.Calls))
}

func summarize(calls []messenger.MockCall) string {
	var b strings.Builder
	for _, c := range calls {
		b.WriteString("[")
		b.WriteString(c.Method)
		b.WriteString("] ")
		b.WriteString(c.Text)
		b.WriteString(" | ")
	}
	return b.String()
}

// lastBotMessage finds the most recent bot-outbound message (Send or force-
// reply). DeleteMessage entries don't have user-visible text and are skipped.
func lastBotMessage(calls []messenger.MockCall) (messenger.MockCall, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		switch calls[i].Method {
		case "SendMessage", "SendMessageWithForceReply":
			return calls[i], true
		}
	}
	return messenger.MockCall{}, false
}

func hasDeleteCall(calls []messenger.MockCall, chat, msg int64) bool {
	for _, c := range calls {
		if c.Method == "DeleteMessage" && c.ChatID == chat && c.MessageID == msg {
			return true
		}
	}
	return false
}

// ---------------- /start ----------------

func TestStartGreets(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/start")
	w.assertReplyContains("identity bot")
	w.assertReplyContains("/provide_nickname")
	w.assertReplyContains("/login")
}

// ---------------- /login ----------------

func TestLoginRejectsInlineArgs(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/login user:pw")
	w.assertReplyContains("takes no arguments")
	if _, err := w.store.S21Accounts().Get(w.ctx, 100); !errors.Is(err, s21account.ErrNotFound) {
		t.Errorf("no row should be stored on inline-args reject; got err=%v", err)
	}
}

func TestLoginTwoStepStoresAccount(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusID: "kazan", CampusName: "21 Kazan"})
	w.dm(100, "alice", "/login")
	w.assertReplyContains("[LOGIN_OP=set]")
	w.dmReply(100, "alice", "evangelm:secret")
	w.assertReplyContains("logged in as evangelm")

	a, err := w.store.S21Accounts().Get(w.ctx, 100)
	if err != nil {
		t.Fatalf("Get account: %v", err)
	}
	if a.S21Login != "evangelm" || a.CampusName != "21 Kazan" {
		t.Errorf("unexpected stored row: %+v", a)
	}
	pw, err := w.cipher.Decrypt(a.S21CredsEncrypted)
	if err != nil || pw != "secret" {
		t.Errorf("creds round-trip: pw=%q err=%v", pw, err)
	}
	// Bot must delete the user's creds message.
	if !hasDeleteCall(w.M.Calls, 100, 999) {
		t.Errorf("expected DeleteMessage for the user's creds message; got %s", summarize(w.M.Calls))
	}
}

func TestLoginBadCredentials(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/login")
	w.dmReply(100, "alice", "nobody:wrong")
	w.assertReplyContains("rejected")
	if _, err := w.store.S21Accounts().Get(w.ctx, 100); !errors.Is(err, s21account.ErrNotFound) {
		t.Errorf("no row should be stored on bad creds; got err=%v", err)
	}
}

func TestLoginRotatesOwnRow(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("a", "pw", s21.Profile{Login: "a", CampusName: "21 Kazan"})
	w.S21.SetUser("a2", "pw3", s21.Profile{Login: "a2", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "alice", "a", "pw")
	first, _ := w.store.S21Accounts().Get(w.ctx, 100)

	w.advanceClock(time.Minute)
	w.loginViaCommand(100, "alice", "a2", "pw3")
	second, err := w.store.S21Accounts().Get(w.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if second.S21Login != "a2" {
		t.Errorf("expected rotated login a2, got %q", second.S21Login)
	}
	// CreatedAt is preserved across re-login; LastUsedAt advances.
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt should be preserved; first=%v second=%v", first.CreatedAt, second.CreatedAt)
	}
	if first.LastUsedAt == nil || second.LastUsedAt == nil {
		t.Fatalf("LastUsedAt should be set on both rows; first=%v second=%v", first.LastUsedAt, second.LastUsedAt)
	}
	if !second.LastUsedAt.After(*first.LastUsedAt) {
		t.Errorf("LastUsedAt should advance; first=%v second=%v", first.LastUsedAt, second.LastUsedAt)
	}
}

// ---------------- /logout ----------------

func TestLogoutRequiresLoggedIn(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/logout")
	w.assertReplyContains("not logged in")
}

func TestLogoutTwoStepRemovesRow(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("a", "pw", s21.Profile{Login: "a", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "alice", "a", "pw")
	w.dm(100, "alice", "/logout")
	w.assertReplyContains("[LOGIN_OP=logout]")
	w.dmReply(100, "alice", "confirm")
	w.assertReplyContains("Logged out")
	if _, err := w.store.S21Accounts().Get(w.ctx, 100); !errors.Is(err, s21account.ErrNotFound) {
		t.Errorf("row should be gone after confirm; got err=%v", err)
	}
}

func TestLogoutCancelKeepsRow(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("a", "pw", s21.Profile{Login: "a", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "alice", "a", "pw")
	w.dm(100, "alice", "/logout")
	w.dmReply(100, "alice", "nope")
	w.assertReplyContains("Cancelled")
	if _, err := w.store.S21Accounts().Get(w.ctx, 100); err != nil {
		t.Errorf("row should still exist after cancel; got err=%v", err)
	}
}

// ---------------- /whoami ----------------

func TestWhoamiNotLoggedIn(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/whoami")
	w.assertReplyContains("not logged in")
}

func TestWhoamiLoggedIn(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("a", "pw", s21.Profile{Login: "a", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "alice", "a", "pw")
	w.dm(100, "alice", "/whoami")
	w.assertReplyContains("a")
	w.assertReplyContains("21 Kazan")
}

// ---------------- /provide_nickname (needs a healthy logged-in row) ----------------

func TestProvideNicknameNoHealthyAccounts(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/provide_nickname alice_s21")
	w.assertReplyContains("No healthy S21 accounts")
}

func TestProvideNicknameSuccess(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan", CampusID: "kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")

	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.assertReplyContains("registered")
	if got := strings.Join(w.idService.calls, " | "); !strings.Contains(got, "PUT /users/by_telegram/200") {
		t.Errorf("expected PUT call to fake service; got: %s", got)
	}
}

func TestProvideNicknameEmpty(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	w.dm(200, "alice", "/provide_nickname  ")
	w.assertReplyContains("Usage:")
}

// ---------------- /remove_nickname ----------------

func TestRemoveNickname(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.M.Calls = nil
	w.dm(200, "alice", "/remove_nickname")
	w.assertReplyContains("cleared")
}

// ---------------- /my_nickname ----------------

func TestMyNicknameNotRegistered(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	w.dm(200, "alice", "/my_nickname")
	w.assertReplyContains("don't have a nickname registered")
}

func TestMyNicknameSuccess(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.M.Calls = nil
	w.dm(200, "alice", "/my_nickname")
	w.assertReplyContains("Your nickname: alice_s21")
	w.assertReplyContains("21 Kazan")
}

// ---------------- non-DM gating ----------------

func TestGroupChatIgnored(t *testing.T) {
	w := newWorld(t)
	upd := &messenger.Update{
		Message: &messenger.Message{
			Chat: messenger.Chat{ID: -1, Type: "supergroup"},
			From: &messenger.User{ID: 100},
			Text: "/start",
		},
	}
	if err := w.handlers.Dispatch(w.ctx, upd); err != nil {
		t.Fatal(err)
	}
	if len(w.M.Calls) != 0 {
		t.Errorf("group chat should be ignored; got %d replies", len(w.M.Calls))
	}
}

// ---------------- unknown commands ----------------

func TestUnknownCommandIgnored(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/wibble")
	if len(w.M.Calls) != 0 {
		t.Errorf("unknown command should be silent; got %d replies", len(w.M.Calls))
	}
}

// ---------------- periodic cron glue ----------------
//
// These tests cover this bot's WIRING of the shared-package Decision into the
// store + messenger. They don't re-test the milestone / 7-day deadline math —
// that's owned and tested by s21-account-go.

func TestCronHealthyTickIsSilent(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	w.runCron()
	if len(w.M.Calls) != 0 {
		t.Errorf("healthy cron tick should be silent; got %d msgs (%s)", len(w.M.Calls), summarize(w.M.Calls))
	}
	a, _ := w.store.S21Accounts().Get(w.ctx, 100)
	if a.S21CredsFailedAt != nil {
		t.Errorf("S21CredsFailedAt should stay nil on success; got %v", a.S21CredsFailedAt)
	}
}

func TestCronRejectionStampsMarkersAndWarns(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	// Rotate S21 password so the next probe fails.
	w.S21.SetUser("evangelm", "newpw", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.runCron()
	w.assertReplyContains("S21")
	a, _ := w.store.S21Accounts().Get(w.ctx, 100)
	if a.S21CredsFailedAt == nil || a.S21CredsLastWarnedAt == nil {
		t.Errorf("expected both markers set; got failed=%v warned=%v", a.S21CredsFailedAt, a.S21CredsLastWarnedAt)
	}
}

func TestCronAutoLogoutAt7Days(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.loginViaCommand(100, "admin", "evangelm", "secret")
	w.S21.SetUser("evangelm", "newpw", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.runCron()
	w.M.Calls = nil
	w.advanceClock(7*24*time.Hour + time.Minute)
	w.runCron()
	if _, err := w.store.S21Accounts().Get(w.ctx, 100); !errors.Is(err, s21account.ErrNotFound) {
		t.Errorf("row should be deleted after 7d of failure; got err=%v", err)
	}
}
