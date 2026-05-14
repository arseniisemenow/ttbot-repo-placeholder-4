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

func (s *Store) Admin() store.AdminRepo                  { return adminRepo{s} }
func (s *Store) PendingDeletes() store.PendingDeleteRepo { return pendingRepo{s} }

type adminRepo struct{ s *Store }

func (r adminRepo) Get(ctx context.Context) (store.BotAdmin, error) {
	var (
		a        store.BotAdmin
		tid      uint64
		failedAt *time.Time
		warnedAt *time.Time
	)
	err := r.s.driver.Table().Do(ctx, func(ctx context.Context, sess table.Session) error {
		_, res, err := sess.Execute(ctx, table.DefaultTxControl(),
			"SELECT telegram_id, s21_login, s21_creds_encrypted, updated_at, s21_creds_failed_at, s21_creds_last_warned_at FROM bot_admin WHERE id = 1;",
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
			named.Optional("s21_creds_failed_at", &failedAt),
			named.Optional("s21_creds_last_warned_at", &warnedAt),
		); err != nil {
			return err
		}
		a.TelegramID = int64(tid)
		a.S21CredsFailedAt = failedAt
		a.S21CredsLastWarnedAt = warnedAt
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
DECLARE $fat AS Timestamp?;
DECLARE $wat AS Timestamp?;
UPSERT INTO bot_admin (id, telegram_id, s21_login, s21_creds_encrypted, updated_at, s21_creds_failed_at, s21_creds_last_warned_at)
VALUES (1, $tid, $login, $creds, $uat, $fat, $wat);`
	failedAtVal := types.NullValue(types.TypeTimestamp)
	if a.S21CredsFailedAt != nil {
		failedAtVal = types.OptionalValue(types.TimestampValueFromTime(a.S21CredsFailedAt.UTC()))
	}
	warnedAtVal := types.NullValue(types.TypeTimestamp)
	if a.S21CredsLastWarnedAt != nil {
		warnedAtVal = types.OptionalValue(types.TimestampValueFromTime(a.S21CredsLastWarnedAt.UTC()))
	}
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx, sql, table.NewQueryParameters(
			table.ValueParam("$tid", types.Uint64Value(uint64(a.TelegramID))),
			table.ValueParam("$login", types.UTF8Value(a.S21Login)),
			table.ValueParam("$creds", types.UTF8Value(a.S21CredsEncrypted)),
			table.ValueParam("$uat", types.TimestampValueFromTime(a.UpdatedAt.UTC())),
			table.ValueParam("$fat", failedAtVal),
			table.ValueParam("$wat", warnedAtVal),
		))
		return err
	}, table.WithIdempotent(),
		table.WithTxSettings(table.TxSettings(table.WithSerializableReadWrite())))
}

func (r adminRepo) Delete(ctx context.Context) error {
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx, "DELETE FROM bot_admin WHERE id = 1;", nil)
		return err
	}, table.WithIdempotent())
}

// ---------------- pending deletes ----------------

type pendingRepo struct{ s *Store }

func (r pendingRepo) Insert(ctx context.Context, p store.PendingDelete) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	const sql = `
DECLARE $cid AS Int64;
DECLARE $mid AS Int64;
DECLARE $dat AS Timestamp;
DECLARE $cat AS Timestamp;
UPSERT INTO pending_deletes (chat_id, message_id, delete_at, created_at)
VALUES ($cid, $mid, $dat, $cat);`
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx, sql, table.NewQueryParameters(
			table.ValueParam("$cid", types.Int64Value(p.ChatID)),
			table.ValueParam("$mid", types.Int64Value(p.MessageID)),
			table.ValueParam("$dat", types.TimestampValueFromTime(p.DeleteAt.UTC())),
			table.ValueParam("$cat", types.TimestampValueFromTime(p.CreatedAt.UTC())),
		))
		return err
	}, table.WithIdempotent())
}

func (r pendingRepo) ListDue(ctx context.Context, now time.Time) ([]store.PendingDelete, error) {
	var out []store.PendingDelete
	err := r.s.driver.Table().Do(ctx, func(ctx context.Context, sess table.Session) error {
		_, res, err := sess.Execute(ctx, table.DefaultTxControl(),
			`DECLARE $now AS Timestamp;
			 SELECT chat_id, message_id, delete_at, created_at FROM pending_deletes
			 WHERE delete_at <= $now ORDER BY delete_at;`,
			table.NewQueryParameters(
				table.ValueParam("$now", types.TimestampValueFromTime(now.UTC())),
			))
		if err != nil {
			return err
		}
		defer res.Close()
		if err := res.NextResultSetErr(ctx); err != nil {
			return err
		}
		for res.NextRow() {
			var p store.PendingDelete
			if err := res.ScanNamed(
				named.Required("chat_id", &p.ChatID),
				named.Required("message_id", &p.MessageID),
				named.Required("delete_at", &p.DeleteAt),
				named.Required("created_at", &p.CreatedAt),
			); err != nil {
				return err
			}
			out = append(out, p)
		}
		return nil
	}, table.WithIdempotent())
	return out, err
}

func (r pendingRepo) Delete(ctx context.Context, chatID, messageID int64) error {
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx,
			"DECLARE $cid AS Int64; DECLARE $mid AS Int64; DELETE FROM pending_deletes WHERE chat_id = $cid AND message_id = $mid;",
			table.NewQueryParameters(
				table.ValueParam("$cid", types.Int64Value(chatID)),
				table.ValueParam("$mid", types.Int64Value(messageID)),
			))
		return err
	}, table.WithIdempotent())
}
