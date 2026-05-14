package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	identityclient "github.com/arseniisemenow/s21-identity-client-go"
	"github.com/arseniisemenow/s21-identity-bot/pkg/crypto"
	"github.com/arseniisemenow/s21-identity-bot/pkg/handlers"
	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
	"github.com/arseniisemenow/s21-identity-bot/pkg/s21"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store/memstore"
)

// world is the test fixture: a fresh memstore, mock messenger, mock S21,
// fake identity service via httptest, and a fully-wired Handlers.
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
	// clock is the injectable time source. Tests advance it to exercise the
	// cron's time-driven behaviour (creds-failure milestones / auto-unadmin).
	clock time.Time
}

// fakeIdentityService stands in for placeholder-3 in tests.
type fakeIdentityService struct {
	mu     sync.Mutex
	users  map[int64]identityclient.User
	failNext error
	calls  []string
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
			var body struct{ Nickname string `json:"nickname"` }
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

// advanceClock moves the test clock forward by d. Subsequent Cfg.Now()
// reads (in handlers + cron) see the new value.
func (w *world) advanceClock(d time.Duration) {
	w.clock = w.clock.Add(d)
}

// runCron invokes PeriodicJob synchronously. Tests use it together with
// advanceClock to walk the creds-failure timeline.
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

// dmReply dispatches a private-chat message that's a reply to the bot's most
// recent outgoing message. Used to exercise force-reply flows (/admin step 2,
// /unadmin confirm, /new_read_key creds prompt).
func (w *world) dmReply(from int64, username, text string) {
	w.t.Helper()
	if len(w.M.Calls) == 0 {
		w.t.Fatal("no prior bot message to reply to")
	}
	last := w.M.Calls[len(w.M.Calls)-1]
	upd := &messenger.Update{
		Message: &messenger.Message{
			MessageID: 999,
			Chat:      messenger.Chat{ID: from, Type: "private"},
			From:      &messenger.User{ID: from, Username: username},
			Text:      text,
			ReplyTo: &messenger.Message{
				MessageID: int64(len(w.M.Calls)),
				From:      &messenger.User{ID: 0, IsBot: true},
				Text:      last.Text,
			},
		},
	}
	if err := w.handlers.Dispatch(w.ctx, upd); err != nil {
		w.t.Fatalf("dispatch reply: %v", err)
	}
}

// adminViaCommand exercises the two-step /admin flow end-to-end: send the
// command, then reply with credentials. Resets w.M.Calls afterwards so tests
// can assert on subsequent replies cleanly.
func (w *world) adminViaCommand(tid int64, username, login, password string) {
	w.t.Helper()
	w.dm(tid, username, "/admin")
	w.dmReply(tid, username, login+":"+password)
	w.M.Calls = nil
}

// assertReplyContains asserts at least one outbound message contains substr.
func (w *world) assertReplyContains(substr string) {
	w.t.Helper()
	for _, c := range w.M.Calls {
		if strings.Contains(c.Text, substr) {
			return
		}
	}
	w.t.Fatalf("no reply contains %q; got %d replies", substr, len(w.M.Calls))
}

// ---------------- /start ----------------

func TestStartGreets(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/start")
	w.assertReplyContains("identity bot")
	w.assertReplyContains("/provide_nickname")
}

// ---------------- /admin ----------------

func TestAdminSetsRow(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusID: "kazan", CampusName: "21 Kazan"})
	// Two-step: command first, then reply with creds.
	w.dm(100, "alice", "/admin")
	w.assertReplyContains("[ADMIN_OP=set]")
	w.dmReply(100, "alice", "evangelm:secret")
	w.assertReplyContains("now the identity-bot admin")
	a, err := w.store.Admin().Get(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.TelegramID != 100 || a.S21Login != "evangelm" {
		t.Errorf("admin row: %+v", a)
	}
	pw, err := w.cipher.Decrypt(a.S21CredsEncrypted)
	if err != nil || pw != "secret" {
		t.Errorf("decrypt: pw=%q err=%v", pw, err)
	}
	// Bot must delete the user's creds message.
	if !hasDeleteCall(w.M.Calls, 100, 999) {
		t.Errorf("expected DeleteMessage for the user's creds message; got %+v", w.M.Calls)
	}
}

func TestAdminRejectsInlineArgs(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/admin evangelm:secret")
	w.assertReplyContains("Inline credentials are no longer accepted")
	if _, err := w.store.Admin().Get(w.ctx); err == nil {
		t.Error("admin row should not be set when inline args used")
	}
}

func TestAdminBadCredentials(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/admin")
	w.dmReply(100, "alice", "nobody:wrong")
	w.assertReplyContains("rejected")
	if _, err := w.store.Admin().Get(w.ctx); err == nil {
		t.Error("admin row should not be set on bad credentials")
	}
}

// TestAdminFirstWinsByIdentity verifies the same-identity-can-rotate /
// different-identity-rejected rule.
func TestAdminFirstWinsByIdentity(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("a", "pw", s21.Profile{Login: "a", CampusName: "21 Kazan"})
	w.S21.SetUser("b", "pw2", s21.Profile{Login: "b", CampusName: "21 Kazan"})

	// user_a claims the slot.
	w.dm(100, "user_a", "/admin")
	w.dmReply(100, "user_a", "a:pw")
	a, _ := w.store.Admin().Get(w.ctx)
	if a.TelegramID != 100 || a.S21Login != "a" {
		t.Fatalf("first /admin expected to set user_a/100; got %+v", a)
	}

	// user_b tries to take over — rejected before the prompt is sent.
	w.M.Calls = nil
	w.dm(200, "user_b", "/admin")
	w.assertReplyContains("already has an admin")
	a, _ = w.store.Admin().Get(w.ctx)
	if a.TelegramID != 100 || a.S21Login != "a" {
		t.Errorf("expected user_a still admin; got %+v", a)
	}

	// user_a rotates — same telegram_id allowed.
	w.M.Calls = nil
	w.S21.SetUser("a2", "pw3", s21.Profile{Login: "a2", CampusName: "21 Kazan"})
	w.dm(100, "user_a", "/admin")
	w.dmReply(100, "user_a", "a2:pw3")
	a, _ = w.store.Admin().Get(w.ctx)
	if a.TelegramID != 100 || a.S21Login != "a2" {
		t.Errorf("rotation expected a2/100; got %+v", a)
	}
}

// hasDeleteCall reports whether the mock recorded a DeleteMessage for the
// given (chatID, messageID).
func hasDeleteCall(calls []messenger.MockCall, chat, msg int64) bool {
	for _, c := range calls {
		if c.Method == "DeleteMessage" && c.ChatID == chat && c.MessageID == msg {
			return true
		}
	}
	return false
}

// ---------------- /provide_nickname ----------------

func TestProvideNicknameRequiresAdmin(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/provide_nickname alice_s21")
	w.assertReplyContains("has no admin set")
}

func TestProvideNicknameSuccess(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan", CampusID: "kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.M.Calls = nil
	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.assertReplyContains("registered")
	// Verify fake service got a PUT call.
	got := strings.Join(w.idService.calls, " | ")
	if !strings.Contains(got, "PUT /users/by_telegram/200") {
		t.Errorf("expected PUT call to fake service; got: %s", got)
	}
}

func TestProvideNicknameEmpty(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.M.Calls = nil
	w.dm(200, "alice", "/provide_nickname  ")
	w.assertReplyContains("Usage:")
}

// ---------------- /remove_nickname ----------------

func TestRemoveNickname(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.M.Calls = nil
	w.dm(200, "alice", "/remove_nickname")
	w.assertReplyContains("cleared")
}

// ---------------- /my_nickname ----------------

func TestMyNicknameNotRegistered(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.M.Calls = nil
	w.dm(200, "alice", "/my_nickname")
	w.assertReplyContains("don't have a nickname registered")
}

func TestMyNicknameSuccess(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.M.Calls = nil
	w.dm(200, "alice", "/my_nickname")
	w.assertReplyContains("Your nickname: alice_s21")
	w.assertReplyContains("21 Kazan")
}

// ---------------- /list_users ----------------

func TestListUsersRejectsNonAdmin(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.M.Calls = nil
	w.dm(999, "rando", "/list_users")
	w.assertReplyContains("Only the identity-bot admin")
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

// ---------------- Unknown commands ----------------

func TestUnknownCommandIgnored(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/wibble")
	if len(w.M.Calls) != 0 {
		t.Errorf("unknown command should be silent; got %d replies", len(w.M.Calls))
	}
}

// ---------------- creds-failure cron path ----------------

// breakAdminCreds rotates the password upstream so the next re-auth fails
// with ErrInvalidCredentials (the password stored in bot_admin no longer
// matches what the s21 Mock expects).
func (w *world) breakAdminCreds(login, newPassword string) {
	w.S21.SetUser(login, newPassword, s21.Profile{Login: login, CampusName: "21 Kazan"})
}

// TestCredsHealthyTickIsSilent asserts the cron does nothing when S21 is
// happy: no DM, no row mutation.
func TestCredsHealthyTickIsSilent(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.runCron()
	if len(w.M.Calls) != 0 {
		t.Errorf("healthy cron tick should be silent; got %d msgs", len(w.M.Calls))
	}
	a, _ := w.store.Admin().Get(w.ctx)
	if a.S21CredsFailedAt != nil {
		t.Errorf("S21CredsFailedAt should stay nil on success; got %v", a.S21CredsFailedAt)
	}
}

// TestCredsFirstFailureSetsMarkersAndWarns asserts the first failure stamps
// both markers and DMs the admin once.
func TestCredsFirstFailureSetsMarkersAndWarns(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.breakAdminCreds("evangelm", "newpw")
	w.runCron()
	w.assertReplyContains("S21 just rejected")
	a, _ := w.store.Admin().Get(w.ctx)
	if a.S21CredsFailedAt == nil || a.S21CredsLastWarnedAt == nil {
		t.Errorf("expected both markers set; got failed=%v warned=%v",
			a.S21CredsFailedAt, a.S21CredsLastWarnedAt)
	}
}

// TestCredsRepeatedFailureNoSpam asserts the cron does NOT re-DM on the next
// tick when no milestone has been crossed.
func TestCredsRepeatedFailureNoSpam(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.breakAdminCreds("evangelm", "newpw")
	w.runCron() // first DM
	w.M.Calls = nil
	// 15 minutes elapse, still failing — should NOT DM again.
	w.advanceClock(15 * time.Minute)
	w.runCron()
	if len(w.M.Calls) != 0 {
		t.Errorf("expected silence within a milestone; got %d msgs", len(w.M.Calls))
	}
}

// TestCredsMilestoneAt1Day asserts that crossing the 24h milestone triggers
// the next warning DM.
func TestCredsMilestoneAt1Day(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.breakAdminCreds("evangelm", "newpw")
	w.runCron() // first DM at t=0
	w.M.Calls = nil
	w.advanceClock(24*time.Hour + time.Minute)
	w.runCron()
	w.assertReplyContains("rejected your stored credentials for 1d")
}

// TestCredsAutoUnadminAt7Days asserts that 7 days of failure clears the
// admin row and emits the final DM.
func TestCredsAutoUnadminAt7Days(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.breakAdminCreds("evangelm", "newpw")
	w.runCron()
	w.M.Calls = nil
	w.advanceClock(7*24*time.Hour + time.Minute)
	w.runCron()
	if _, err := w.store.Admin().Get(w.ctx); err == nil {
		t.Error("admin row should be deleted after 7d of failure")
	}
	w.assertReplyContains("You are no longer the identity-bot admin")
}

// TestCredsRecoveryClearsMarkers asserts that one successful auth after a
// failure run wipes both markers (back to healthy state).
func TestCredsRecoveryClearsMarkers(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.adminViaCommand(100, "admin", "evangelm", "secret")
	w.breakAdminCreds("evangelm", "newpw")
	w.runCron()
	// Restore the original password.
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.advanceClock(time.Hour)
	w.runCron()
	a, _ := w.store.Admin().Get(w.ctx)
	if a.S21CredsFailedAt != nil || a.S21CredsLastWarnedAt != nil {
		t.Errorf("recovery should clear markers; got failed=%v warned=%v",
			a.S21CredsFailedAt, a.S21CredsLastWarnedAt)
	}
}
