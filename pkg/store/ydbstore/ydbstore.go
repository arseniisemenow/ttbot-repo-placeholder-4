// Package ydbstore is the YDB-backed store.Store for the identity bot.
package ydbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/result/named"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/types"
	yc "github.com/ydb-platform/ydb-go-yc-metadata"

	"github.com/arseniisemenow/ttbot-repo-placeholder-4/pkg/store"
)

// Store is the YDB-backed store.Store.
type Store struct {
	driver *ydb.Driver
}

// Open connects to YDB via Cloud Function metadata auth.
func Open(ctx context.Context, connectionString string) (*Store, error) {
	d, err := ydb.Open(ctx, connectionString, yc.WithCredentials(), yc.WithInternalCA())
	if err != nil {
		return nil, fmt.Errorf("ydbstore.Open: %w", err)
	}
	return &Store{driver: d}, nil
}

// Close shuts down the driver.
func (s *Store) Close() error {
	if s.driver == nil {
		return nil
	}
	return s.driver.Close(context.Background())
}

func (s *Store) Admin() store.AdminRepo { return adminRepo{s} }

type adminRepo struct{ s *Store }

func (r adminRepo) Get(ctx context.Context) (store.BotAdmin, error) {
	var (
		a   store.BotAdmin
		tid uint64
	)
	err := r.s.driver.Table().Do(ctx, func(ctx context.Context, sess table.Session) error {
		_, res, err := sess.Execute(ctx, table.DefaultTxControl(),
			"SELECT telegram_id, s21_login, s21_creds_encrypted, updated_at FROM bot_admin WHERE id = 1;",
			nil)
		if err != nil {
			return err
		}
		defer res.Close()
		if err := res.NextResultSetErr(ctx); err != nil {
			return err
		}
		if !res.NextRow() {
			return store.ErrNotFound
		}
		if err := res.ScanNamed(
			named.Required("telegram_id", &tid),
			named.Required("s21_login", &a.S21Login),
			named.Required("s21_creds_encrypted", &a.S21CredsEncrypted),
			named.Required("updated_at", &a.UpdatedAt),
		); err != nil {
			return err
		}
		a.TelegramID = int64(tid)
		return nil
	}, table.WithIdempotent())
	if errors.Is(err, store.ErrNotFound) {
		return store.BotAdmin{}, err
	}
	return a, err
}

func (r adminRepo) Set(ctx context.Context, a store.BotAdmin) error {
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = time.Now().UTC()
	}
	const sql = `
DECLARE $tid AS Uint64;
DECLARE $login AS Utf8;
DECLARE $creds AS Utf8;
DECLARE $uat AS Timestamp;
UPSERT INTO bot_admin (id, telegram_id, s21_login, s21_creds_encrypted, updated_at)
VALUES (1, $tid, $login, $creds, $uat);`
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx, sql, table.NewQueryParameters(
			table.ValueParam("$tid", types.Uint64Value(uint64(a.TelegramID))),
			table.ValueParam("$login", types.UTF8Value(a.S21Login)),
			table.ValueParam("$creds", types.UTF8Value(a.S21CredsEncrypted)),
			table.ValueParam("$uat", types.TimestampValueFromTime(a.UpdatedAt.UTC())),
		))
		return err
	}, table.WithIdempotent(),
		table.WithTxSettings(table.TxSettings(table.WithSerializableReadWrite())))
}
