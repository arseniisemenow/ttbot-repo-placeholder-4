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

	s21account "github.com/arseniisemenow/s21-account-go"
	"github.com/arseniisemenow/s21-identity-bot/pkg/store"
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
func (s *Store) S21Accounts() store.S21AccountRepo       { return s21AccountRepo{s} }

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

// ---------------- s21_accounts ----------------

type s21AccountRepo struct{ s *Store }

const s21AccountColsSel = `telegram_id, s21_login, s21_creds_encrypted, campus_id, campus_name, created_at, updated_at, last_used_at, s21_creds_failed_at, s21_creds_last_warned_at`

func scanS21Account(res interface {
	ScanNamed(...named.Value) error
}) (store.S21Account, error) {
	var (
		a               store.S21Account
		tid             uint64
		campusID        *string
		campusName      *string
		lastUsedAt      *time.Time
		failedAt        *time.Time
		lastWarnedAt    *time.Time
	)
	if err := res.ScanNamed(
		named.Required("telegram_id", &tid),
		named.Required("s21_login", &a.S21Login),
		named.Required("s21_creds_encrypted", &a.S21CredsEncrypted),
		named.Optional("campus_id", &campusID),
		named.Optional("campus_name", &campusName),
		named.Required("created_at", &a.CreatedAt),
		named.Required("updated_at", &a.UpdatedAt),
		named.Optional("last_used_at", &lastUsedAt),
		named.Optional("s21_creds_failed_at", &failedAt),
		named.Optional("s21_creds_last_warned_at", &lastWarnedAt),
	); err != nil {
		return store.S21Account{}, err
	}
	a.TelegramID = int64(tid)
	if campusID != nil {
		a.CampusID = *campusID
	}
	if campusName != nil {
		a.CampusName = *campusName
	}
	a.LastUsedAt = lastUsedAt
	a.S21CredsFailedAt = failedAt
	a.S21CredsLastWarnedAt = lastWarnedAt
	return a, nil
}

func (r s21AccountRepo) Get(ctx context.Context, tid int64) (store.S21Account, error) {
	var a store.S21Account
	err := r.s.driver.Table().Do(ctx, func(ctx context.Context, sess table.Session) error {
		_, res, err := sess.Execute(ctx, table.DefaultTxControl(),
			"DECLARE $tid AS Uint64; SELECT "+s21AccountColsSel+" FROM s21_accounts WHERE telegram_id = $tid;",
			table.NewQueryParameters(table.ValueParam("$tid", types.Uint64Value(uint64(tid)))))
		if err != nil {
			return err
		}
		defer res.Close()
		if err := res.NextResultSetErr(ctx); err != nil {
			return err
		}
		if !res.NextRow() {
			return s21account.ErrNotFound
		}
		a, err = scanS21Account(res)
		return err
	}, table.WithIdempotent())
	if errors.Is(err, s21account.ErrNotFound) {
		return store.S21Account{}, err
	}
	return a, err
}

func (r s21AccountRepo) List(ctx context.Context) ([]store.S21Account, error) {
	var out []store.S21Account
	err := r.s.driver.Table().Do(ctx, func(ctx context.Context, sess table.Session) error {
		_, res, err := sess.Execute(ctx, table.DefaultTxControl(),
			"SELECT "+s21AccountColsSel+" FROM s21_accounts ORDER BY created_at, telegram_id;", nil)
		if err != nil {
			return err
		}
		defer res.Close()
		if err := res.NextResultSetErr(ctx); err != nil {
			return err
		}
		for res.NextRow() {
			a, err := scanS21Account(res)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return nil
	}, table.WithIdempotent())
	return out, err
}

func (r s21AccountRepo) Upsert(ctx context.Context, a store.S21Account) error {
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = time.Now().UTC()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = a.UpdatedAt
	}
	optStr := func(s string) types.Value {
		if s == "" {
			return types.NullValue(types.TypeUTF8)
		}
		return types.OptionalValue(types.UTF8Value(s))
	}
	optTime := func(t *time.Time) types.Value {
		if t == nil {
			return types.NullValue(types.TypeTimestamp)
		}
		return types.OptionalValue(types.TimestampValueFromTime(t.UTC()))
	}
	const sql = `
DECLARE $tid AS Uint64;
DECLARE $login AS Utf8;
DECLARE $creds AS Utf8;
DECLARE $cid AS Utf8?;
DECLARE $cname AS Utf8?;
DECLARE $cat AS Timestamp;
DECLARE $uat AS Timestamp;
DECLARE $lua AS Timestamp?;
DECLARE $fat AS Timestamp?;
DECLARE $wat AS Timestamp?;
UPSERT INTO s21_accounts
(telegram_id, s21_login, s21_creds_encrypted, campus_id, campus_name,
 created_at, updated_at, last_used_at, s21_creds_failed_at, s21_creds_last_warned_at)
VALUES ($tid, $login, $creds, $cid, $cname, $cat, $uat, $lua, $fat, $wat);`
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx, sql, table.NewQueryParameters(
			table.ValueParam("$tid", types.Uint64Value(uint64(a.TelegramID))),
			table.ValueParam("$login", types.UTF8Value(a.S21Login)),
			table.ValueParam("$creds", types.UTF8Value(a.S21CredsEncrypted)),
			table.ValueParam("$cid", optStr(a.CampusID)),
			table.ValueParam("$cname", optStr(a.CampusName)),
			table.ValueParam("$cat", types.TimestampValueFromTime(a.CreatedAt.UTC())),
			table.ValueParam("$uat", types.TimestampValueFromTime(a.UpdatedAt.UTC())),
			table.ValueParam("$lua", optTime(a.LastUsedAt)),
			table.ValueParam("$fat", optTime(a.S21CredsFailedAt)),
			table.ValueParam("$wat", optTime(a.S21CredsLastWarnedAt)),
		))
		return err
	}, table.WithIdempotent())
}

func (r s21AccountRepo) Delete(ctx context.Context, tid int64) error {
	return r.s.driver.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) error {
		_, err := tx.Execute(ctx,
			"DECLARE $tid AS Uint64; DELETE FROM s21_accounts WHERE telegram_id = $tid;",
			table.NewQueryParameters(table.ValueParam("$tid", types.Uint64Value(uint64(tid)))))
		return err
	}, table.WithIdempotent())
}
