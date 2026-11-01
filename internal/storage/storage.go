// Package storage — хранилище подписчиков, сообщений и событий в PostgreSQL.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusActive       = "active"
	StatusUnsubscribed = "unsubscribed"
	StatusBlocked      = "blocked"
)

var (
	ErrNotFound = errors.New("storage: not found")
	ErrLinkUsed = errors.New("storage: registration link already used")
)

// Subscriber — получатель из таблицы recipients: добавленный вручную или по ссылке регистрации.
type Subscriber struct {
	ID         int64
	TelegramID int64
	Username   string
	FirstName  string
	LastName   string
	Token      uuid.UUID
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const subscriberColumns = `id, tg_id, username, first_name, last_name, token, status, created_at, updated_at`

func scanSubscriber(row pgx.Row, extra ...any) (Subscriber, error) {
	var s Subscriber
	dest := append([]any{&s.ID, &s.TelegramID, &s.Username, &s.FirstName, &s.LastName,
		&s.Token, &s.Status, &s.CreatedAt, &s.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Subscriber{}, ErrNotFound
		}
		return Subscriber{}, err
	}
	return s, nil
}

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Profile — данные пользователя Telegram.
type Profile struct {
	TelegramID int64
	Username   string
	FirstName  string
	LastName   string
}

type Registration struct {
	Profile
	Mode          string
	Nonce         []byte
	LinkExpiresAt time.Time

	// Заполняются только в режиме callback.
	CallbackCodeHash  []byte
	CallbackExpiresAt time.Time
}

// Register добавляет получателя или реактивирует существующего и помечает nonce ссылки
// использованным. Токен и псевдоним существующего получателя не меняются. Повторное
// использование ссылки тем же пользователем допускается (повторный /start), другим —
// возвращает ErrLinkUsed.
func (s *Store) Register(ctx context.Context, r Registration) (Subscriber, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Subscriber{}, err
	}
	defer tx.Rollback(ctx)

	sub, err := upsertRecipient(ctx, tx, r.Profile)
	if err != nil {
		return Subscriber{}, err
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO registration_nonces (nonce, mode, subscriber_id, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (nonce) DO UPDATE SET used_at = registration_nonces.used_at
		WHERE registration_nonces.subscriber_id = EXCLUDED.subscriber_id`,
		r.Nonce, r.Mode, sub.ID, r.LinkExpiresAt)
	if err != nil {
		return Subscriber{}, fmt.Errorf("storage: save nonce: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Subscriber{}, ErrLinkUsed
	}

	if r.CallbackCodeHash != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO callback_codes (code_hash, subscriber_id, nonce, expires_at)
			VALUES ($1, $2, $3, $4)`,
			r.CallbackCodeHash, sub.ID, r.Nonce, r.CallbackExpiresAt); err != nil {
			return Subscriber{}, fmt.Errorf("storage: save callback code: %w", err)
		}
	}

	if err := addEvent(ctx, tx, sub.ID, "registered", map[string]any{
		"mode":  r.Mode,
		"nonce": hex.EncodeToString(r.Nonce),
	}); err != nil {
		return Subscriber{}, err
	}
	return sub, tx.Commit(ctx)
}

// AddRecipient добавляет получателя без ссылки регистрации. Токен и псевдоним существующего
// получателя не меняются. mode попадает в журнал событий.
func (s *Store) AddRecipient(ctx context.Context, p Profile, mode string) (Subscriber, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Subscriber{}, err
	}
	defer tx.Rollback(ctx)

	sub, err := upsertRecipient(ctx, tx, p)
	if err != nil {
		return Subscriber{}, err
	}
	if err := addEvent(ctx, tx, sub.ID, "registered", map[string]any{"mode": mode}); err != nil {
		return Subscriber{}, err
	}
	return sub, tx.Commit(ctx)
}

func upsertRecipient(ctx context.Context, tx pgx.Tx, p Profile) (Subscriber, error) {
	sub, err := scanSubscriber(tx.QueryRow(ctx, `
		INSERT INTO recipients (tg_id, username, first_name, last_name, token)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tg_id) DO UPDATE SET
			username   = EXCLUDED.username,
			first_name = EXCLUDED.first_name,
			last_name  = EXCLUDED.last_name,
			status     = 'active',
			updated_at = now()
		RETURNING `+subscriberColumns,
		p.TelegramID, p.Username, p.FirstName, p.LastName, uuid.New()))
	if err != nil {
		return Subscriber{}, fmt.Errorf("storage: upsert subscriber: %w", err)
	}
	return sub, nil
}

// HashCallbackCode — в БД хранится только хеш одноразового кода.
func HashCallbackCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// RedeemCallbackCode гасит одноразовый код и возвращает подписчика и nonce ссылки регистрации.
func (s *Store) RedeemCallbackCode(ctx context.Context, codeHash []byte) (Subscriber, []byte, error) {
	var nonce []byte
	sub, err := scanSubscriber(s.pool.QueryRow(ctx, `
		WITH c AS (
			UPDATE callback_codes SET redeemed_at = now()
			WHERE code_hash = $1 AND redeemed_at IS NULL AND expires_at > now()
			RETURNING subscriber_id, nonce
		)
		SELECT `+prefixed("s.", subscriberColumns)+`, c.nonce
		FROM c JOIN recipients s ON s.id = c.subscriber_id`, codeHash), &nonce)
	if err != nil {
		return Subscriber{}, nil, err
	}
	if err := addEvent(ctx, s.pool, sub.ID, "callback_redeemed", map[string]any{
		"nonce": hex.EncodeToString(nonce),
	}); err != nil {
		return Subscriber{}, nil, err
	}
	return sub, nonce, nil
}

func (s *Store) SubscriberByTelegramID(ctx context.Context, telegramID int64) (Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx,
		`SELECT `+subscriberColumns+` FROM recipients WHERE tg_id = $1`, telegramID))
}

func (s *Store) SubscriberByToken(ctx context.Context, token uuid.UUID) (Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx,
		`SELECT `+subscriberColumns+` FROM recipients WHERE token = $1`, token))
}

// SetStatus меняет статус подписчика и пишет событие status_changed.
func (s *Store) SetStatus(ctx context.Context, subscriberID int64, status, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var prev string
	err = tx.QueryRow(ctx, `
		UPDATE recipients s SET status = $2, updated_at = now()
		FROM (SELECT id, status FROM recipients WHERE id = $1 FOR UPDATE) old
		WHERE s.id = old.id
		RETURNING old.status`, subscriberID, status).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if prev == status {
		return nil
	}
	if status != StatusActive {
		// Неактивному подписчику доставлять нечего: гасим очередь, кроме сообщений в работе.
		if _, err := tx.Exec(ctx, `
			UPDATE messages SET status = 'failed', error = 'subscriber is ' || $2::text, updated_at = now()
			WHERE subscriber_id = $1 AND status = 'pending'
			  AND (locked_until IS NULL OR locked_until < now())`, subscriberID, status); err != nil {
			return fmt.Errorf("storage: cancel pending messages: %w", err)
		}
	}
	if err := addEvent(ctx, tx, subscriberID, "status_changed", map[string]any{
		"from": prev, "to": status, "reason": reason,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// execer — общее у *pgxpool.Pool и pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func addEvent(ctx context.Context, db execer, subscriberID int64, kind string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := db.Exec(ctx, `INSERT INTO events (subscriber_id, kind, details) VALUES ($1, $2, $3)`,
		subscriberID, kind, details)
	if err != nil {
		return fmt.Errorf("storage: add event %s: %w", kind, err)
	}
	return nil
}

func prefixed(prefix, columns string) string {
	cols := strings.Split(columns, ", ")
	for i, c := range cols {
		cols[i] = prefix + c
	}
	return strings.Join(cols, ", ")
}
