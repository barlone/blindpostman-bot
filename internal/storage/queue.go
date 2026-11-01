package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Очередь доставки поверх таблицы messages.
//
// Гарантии:
//   - сообщения одному подписчику уходят строго по порядку и по одному: забирается только
//     «голова» очереди подписчика и только если она не в работе;
//   - между сообщениями в один чат выдерживается chatInterval (по subscribers.last_sent_at),
//     в том числе между несколькими репликами;
//   - аренда (locked_until) защищает от двойной обработки; если воркер упал, после её истечения
//     сообщение заберут снова. Доставка — at-least-once.

const (
	MessagePending = "pending"
	MessageSent    = "sent"
	MessageFailed  = "failed"
	MessageExpired = "expired"

	// NotifyChannel — канал LISTEN/NOTIFY, в который триггер пишет при постановке сообщения.
	NotifyChannel = "blind_postman_messages"
)

type NewMessage struct {
	SubscriberID        int64
	Text                string
	ParseMode           string
	DisableNotification bool
	ExpiresAt           time.Time // нулевое — без срока
}

// EnqueueMessage ставит сообщение в очередь.
func (s *Store) EnqueueMessage(ctx context.Context, m NewMessage) (int64, error) {
	var expires *time.Time
	if !m.ExpiresAt.IsZero() {
		expires = &m.ExpiresAt
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO messages (subscriber_id, text, parse_mode, disable_notification, expires_at, status)
		VALUES ($1, $2, $3, $4, $5, 'pending') RETURNING id`,
		m.SubscriberID, m.Text, m.ParseMode, m.DisableNotification, expires).Scan(&id)
	return id, err
}

// Message — сообщение со статусом доставки.
type Message struct {
	ID                int64
	TelegramID        int64
	Status            string
	Attempts          int
	Error             *string
	TelegramMessageID *int64
	CreatedAt         time.Time
	NextAttemptAt     time.Time
	SentAt            *time.Time
	ExpiresAt         *time.Time
}

func (s *Store) MessageByID(ctx context.Context, id int64) (Message, error) {
	var m Message
	err := s.pool.QueryRow(ctx, `
		SELECT m.id, s.telegram_id, m.status, m.attempts, m.error, m.tg_message_id,
		       m.created_at, m.next_attempt_at, m.sent_at, m.expires_at
		FROM messages m JOIN subscribers s ON s.id = m.subscriber_id
		WHERE m.id = $1`, id).Scan(&m.ID, &m.TelegramID, &m.Status, &m.Attempts, &m.Error,
		&m.TelegramMessageID, &m.CreatedAt, &m.NextAttemptAt, &m.SentAt, &m.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// ClaimedMessage — сообщение, взятое воркером в работу.
type ClaimedMessage struct {
	ID                  int64
	SubscriberID        int64
	TelegramID          int64
	SubscriberStatus    string
	Text                string
	ParseMode           string
	DisableNotification bool
	Attempts            int // с учётом текущей попытки
	ExpiresAt           *time.Time
}

// ClaimMessages берёт в работу до limit готовых к отправке сообщений и арендует их на lease.
func (s *Store) ClaimMessages(ctx context.Context, limit int, lease, chatInterval time.Duration) ([]ClaimedMessage, error) {
	rows, err := s.pool.Query(ctx, `
		WITH heads AS (
			SELECT DISTINCT ON (subscriber_id) id
			FROM messages
			WHERE status = 'pending'
			ORDER BY subscriber_id, id
		),
		claimable AS (
			SELECT m.id
			FROM messages m
			JOIN heads h ON h.id = m.id
			JOIN subscribers s ON s.id = m.subscriber_id
			WHERE m.status = 'pending'
			  AND m.next_attempt_at <= now()
			  AND (m.locked_until IS NULL OR m.locked_until < now())
			  AND (s.last_sent_at IS NULL OR s.last_sent_at <= now() - $2::bigint * interval '1 millisecond')
			ORDER BY m.next_attempt_at, m.id
			LIMIT $1
			FOR UPDATE OF m SKIP LOCKED
		)
		UPDATE messages m
		SET attempts     = m.attempts + 1,
		    locked_until = now() + $3::bigint * interval '1 millisecond',
		    updated_at   = now()
		FROM claimable c, subscribers s
		WHERE m.id = c.id AND s.id = m.subscriber_id
		RETURNING m.id, m.subscriber_id, s.telegram_id, s.status, m.text, m.parse_mode,
		          m.disable_notification, m.attempts, m.expires_at`,
		limit, chatInterval.Milliseconds(), lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("storage: claim messages: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClaimedMessage, error) {
		var m ClaimedMessage
		err := row.Scan(&m.ID, &m.SubscriberID, &m.TelegramID, &m.SubscriberStatus, &m.Text,
			&m.ParseMode, &m.DisableNotification, &m.Attempts, &m.ExpiresAt)
		return m, err
	})
}

// CompleteMessage отмечает сообщение доставленным.
func (s *Store) CompleteMessage(ctx context.Context, m ClaimedMessage, tgMessageID int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE messages
		SET status = 'sent', tg_message_id = $2, sent_at = now(), locked_until = NULL,
		    error = NULL, updated_at = now()
		WHERE id = $1`, m.ID, tgMessageID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE subscribers SET last_sent_at = now() WHERE id = $1`,
		m.SubscriberID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinishMessage переводит сообщение в конечный статус failed или expired.
func (s *Store) FinishMessage(ctx context.Context, id int64, status, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE messages SET status = $2, error = $3, locked_until = NULL, updated_at = now()
		WHERE id = $1`, id, status, reason)
	return err
}

// RetryMessage возвращает сообщение в очередь с задержкой.
// countAttempt=false — попытка не засчитывается (например, при лимите Telegram или остановке).
func (s *Store) RetryMessage(ctx context.Context, id int64, delay time.Duration, reason string, countAttempt bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE messages
		SET next_attempt_at = now() + $2::bigint * interval '1 millisecond',
		    attempts        = attempts - CASE WHEN $4::boolean THEN 0 ELSE 1 END,
		    error           = NULLIF($3::text, ''),
		    locked_until    = NULL,
		    updated_at      = now()
		WHERE id = $1`, id, delay.Milliseconds(), reason, countAttempt)
	return err
}

// ExpireMessages переводит в expired ожидающие сообщения с истёкшим сроком.
func (s *Store) ExpireMessages(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE messages SET status = 'expired', locked_until = NULL, updated_at = now()
		WHERE status = 'pending' AND expires_at <= now()
		  AND (locked_until IS NULL OR locked_until < now())`)
	return tag.RowsAffected(), err
}

// ListenMessages блокируется, вызывая notify на каждое уведомление о новом сообщении.
// Возвращает ошибку при разрыве соединения или отмене ctx.
func (s *Store) ListenMessages(ctx context.Context, notify func()) error {
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// Соединение с активным LISTEN не возвращаем в пул.
	conn := pc.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))

	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		notify()
	}
}
