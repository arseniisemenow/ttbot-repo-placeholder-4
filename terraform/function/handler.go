// Package main is the identity-bot Cloud Function entrypoint. It validates
// the Telegram secret-token header, parses the inbound update, and
// dispatches to pkg/handlers.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"

	"github.com/arseniisemenow/s21-identity-bot/pkg/crypto"
	"github.com/arseniisemenow/s21-identity-bot/pkg/handlers"
	"github.com/arseniisemenow/s21-identity-bot/pkg/messenger"
	"github.com/arseniisemenow/s21-identity-bot/pkg/s21"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store/memstore"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store/ydbstore"
)

type APIGatewayRequest struct {
	HTTPMethod string            `json:"httpMethod"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

type APIGatewayResponse struct {
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
}

// telegramUpdate mirrors the subset of Telegram's update shape we read.
type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message,omitempty"`
}

type telegramMessage struct {
	MessageID int64            `json:"message_id"`
	Chat      telegramChat     `json:"chat"`
	From      *telegramUser    `json:"from"`
	Text      string           `json:"text"`
	ReplyTo   *telegramMessage `json:"reply_to_message,omitempty"`
}

type telegramChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Username string `json:"username"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

var (
	initOnce sync.Once
	hd       *handlers.Handlers
	initErr  error
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("missing env var: %s", name)
	}
	return v
}

func bootstrap() {
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	encKey := mustEnv("ADMIN_CREDENTIAL_ENCRYPTION_KEY")
	identityURL := mustEnv("IDENTITY_SERVICE_URL")
	// IDENTITY_SERVICE_API_KEY is the bot's own write-scope X-Api-Key for
	// /admin/keys calls. Optional in env so a brand-new deploy doesn't
	// hard-fail before the operator runs the CLI; commands that need it
	// will fail soft with a clear message at request time.
	apiKey := os.Getenv("IDENTITY_SERVICE_API_KEY")
	cipher, err := crypto.New(encKey)
	if err != nil {
		initErr = err
		return
	}
	mes := messenger.NewTelegram(botToken)
	s21c := s21.NewClient()
	var st store.Store
	if ep := os.Getenv("YDB_ENDPOINT"); ep != "" {
		yds, err := ydbstore.Open(context.Background(), ep)
		if err != nil {
			log.Printf("ydbstore.Open failed (%v); falling back to memstore", err)
			st = memstore.New()
		} else {
			st = yds
		}
	} else {
		st = memstore.New()
	}
	hd = handlers.New(st, mes, s21c, cipher, handlers.Config{
		IdentityBaseURL:       identityURL,
		IdentityServiceAPIKey: apiKey,
	})
}

// Handler is the Yandex Cloud Function entrypoint.
func Handler(ctx context.Context, req *APIGatewayRequest) (*APIGatewayResponse, error) {
	initOnce.Do(bootstrap)
	if initErr != nil {
		return &APIGatewayResponse{StatusCode: 200, Body: "ok"}, nil
	}

	expected := os.Getenv("TELEGRAM_WEBHOOK_SECRET")
	if expected != "" {
		got := req.Headers["X-Telegram-Bot-Api-Secret-Token"]
		if got == "" {
			got = req.Headers["x-telegram-bot-api-secret-token"]
		}
		if got != expected {
			return &APIGatewayResponse{StatusCode: 401, Body: "unauthorized"}, nil
		}
	}

	var upd telegramUpdate
	if err := json.Unmarshal([]byte(req.Body), &upd); err != nil {
		log.Printf("decode update: %v; body=%s", err, req.Body)
		return &APIGatewayResponse{StatusCode: 200, Body: "ok"}, nil
	}
	mUpd := translate(&upd)
	if mUpd != nil {
		if err := hd.Dispatch(ctx, mUpd); err != nil {
			log.Printf("dispatch: %v", err)
		}
	}
	return &APIGatewayResponse{StatusCode: 200, Body: "ok"}, nil
}

func translate(u *telegramUpdate) *messenger.Update {
	if u == nil || u.Message == nil {
		return nil
	}
	return &messenger.Update{UpdateID: u.UpdateID, Message: translateMessage(u.Message)}
}

func translateMessage(m *telegramMessage) *messenger.Message {
	if m == nil {
		return nil
	}
	out := &messenger.Message{
		MessageID: m.MessageID,
		Chat:      messenger.Chat{ID: m.Chat.ID, Type: m.Chat.Type, Username: m.Chat.Username},
		Text:      m.Text,
	}
	if m.From != nil {
		out.From = &messenger.User{
			ID:        m.From.ID,
			IsBot:     m.From.IsBot,
			Username:  m.From.Username,
			FirstName: m.From.FirstName,
			LastName:  m.From.LastName,
		}
	}
	if m.ReplyTo != nil {
		out.ReplyTo = translateMessage(m.ReplyTo)
	}
	return out
}

// main is a stub. Yandex Go runtime invokes Handler via reflection.
func main() {}
