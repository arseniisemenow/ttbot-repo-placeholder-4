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

	identityclient "github.com/arseniisemenow/ttbot-repo-placeholder-2"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/crypto"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/handlers"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/messenger"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/s21"
	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store/memstore"
)

// world is the test fixture: a fresh memstore, mock messenger, mock S21,
// fake identity service via httptest, and a fully-wired Handlers.
type world struct {
	t        *testing.T
	ctx      context.Context
	store    *memstore.Store
	M        *messenger.Mock
	S21      *s21.Mock
	cipher   *crypto.Cipher
	srv      *httptest.Server
	handlers *handlers.Handlers
	idService *fakeIdentityService
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
	h := handlers.New(st, mes, sm, cipher, handlers.Config{
		IdentityBaseURL: ts.URL,
		Now:             func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	return &world{t: t, ctx: context.Background(), store: st, M: mes, S21: sm, cipher: cipher, srv: ts, handlers: h, idService: idSvc}
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
	w.dm(100, "alice", "/admin evangelm:secret")
	w.assertReplyContains("admin")
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
}

func TestAdminBadCredentials(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "alice", "/admin nobody:wrong")
	w.assertReplyContains("rejected")
	if _, err := w.store.Admin().Get(w.ctx); err == nil {
		t.Error("admin row should not be set on bad credentials")
	}
}

func TestAdminLastWins(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("a", "pw", s21.Profile{Login: "a", CampusName: "21 Kazan"})
	w.S21.SetUser("b", "pw2", s21.Profile{Login: "b", CampusName: "21 Kazan"})
	w.dm(100, "user_a", "/admin a:pw")
	w.dm(200, "user_b", "/admin b:pw2")
	a, _ := w.store.Admin().Get(w.ctx)
	if a.TelegramID != 200 || a.S21Login != "b" {
		t.Errorf("last-wins expected b/200; got %+v", a)
	}
}

func TestAdminMalformedUsage(t *testing.T) {
	w := newWorld(t)
	w.dm(100, "a", "/admin nocolon")
	w.assertReplyContains("Usage:")
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
	w.dm(100, "admin", "/admin evangelm:secret")
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
	w.dm(100, "admin", "/admin evangelm:secret")
	w.M.Calls = nil
	w.dm(200, "alice", "/provide_nickname  ")
	w.assertReplyContains("Usage:")
}

// ---------------- /remove_nickname ----------------

func TestRemoveNickname(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.dm(100, "admin", "/admin evangelm:secret")
	w.dm(200, "alice", "/provide_nickname alice_s21")
	w.M.Calls = nil
	w.dm(200, "alice", "/remove_nickname")
	w.assertReplyContains("cleared")
}

// ---------------- /my_nickname ----------------

func TestMyNicknameNotRegistered(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.dm(100, "admin", "/admin evangelm:secret")
	w.M.Calls = nil
	w.dm(200, "alice", "/my_nickname")
	w.assertReplyContains("don't have a nickname registered")
}

func TestMyNicknameSuccess(t *testing.T) {
	w := newWorld(t)
	w.S21.SetUser("evangelm", "secret", s21.Profile{Login: "evangelm", CampusName: "21 Kazan"})
	w.dm(100, "admin", "/admin evangelm:secret")
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
	w.dm(100, "admin", "/admin evangelm:secret")
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
