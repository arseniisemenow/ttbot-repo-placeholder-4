// Package main is the identity-bot-cron Cloud Function entrypoint. Fired by
// a 15-minute timer, it runs handlers.PeriodicJob: re-validates the primary
// admin's S21 creds and sweeps expired pending_deletes.
package main

import (
	"context"
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

var (
	initOnce sync.Once
	hd       *handlers.Handlers
	initErr  error
)

func bootstrap() {
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	encKey := mustEnv("ADMIN_CREDENTIAL_ENCRYPTION_KEY")
	identityURL := mustEnv("IDENTITY_SERVICE_URL")
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

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("missing env var: %s", name)
	}
	return v
}

// Handler is the Yandex Cloud Function entrypoint, fired by the timer trigger.
func Handler(ctx context.Context, _ any) (any, error) {
	initOnce.Do(bootstrap)
	if initErr != nil {
		log.Printf("bootstrap: %v", initErr)
		return nil, initErr
	}
	if err := hd.PeriodicJob(ctx); err != nil {
		log.Printf("periodic: %v", err)
		return nil, err
	}
	return map[string]string{"status": "ok"}, nil
}

// main is a stub so `go build` is happy.
func main() {}
