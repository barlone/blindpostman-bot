package storage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// Интеграционные тесты на реальном PostgreSQL. Запуск:
//
//	TEST_DATABASE_URL=postgres://postman:postman@localhost:5432/postman?sslmode=disable go test ./internal/storage
//
// Каждый прогон работает в своей временной схеме.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}
	return s
}

func register(t *testing.T, s *Store, telegramID int64, nonce string) Subscriber {
	t.Helper()
	sub, err := s.Register(context.Background(), Registration{
		Profile:       Profile{TelegramID: telegramID, Username: "u"},
		Mode:          "token",
		Nonce:         []byte(nonce),
		LinkExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func enqueue(t *testing.T, s *Store, sub Subscriber, text string) int64 {
	t.Helper()
	id, err := s.EnqueueMessage(context.Background(), NewMessage{SubscriberID: sub.ID, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func claimIDs(t *testing.T, s *Store, chatInterval time.Duration) []int64 {
	t.Helper()
	msgs, err := s.ClaimMessages(context.Background(), 10, time.Minute, chatInterval)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

func TestRegisterLinkReuse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	reg := Registration{Profile: Profile{TelegramID: 1}, Mode: "token", Nonce: []byte("n1"),
		LinkExpiresAt: time.Now().Add(time.Hour)}

	first, err := s.Register(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Register(ctx, reg)
	if err != nil {
		t.Fatalf("same user may reuse own link: %v", err)
	}
	if again.Token != first.Token {
		t.Error("token must survive re-registration")
	}
	reg.TelegramID = 2
	if _, err := s.Register(ctx, reg); !errors.Is(err, ErrLinkUsed) {
		t.Fatalf("other user: got %v, want ErrLinkUsed", err)
	}
}

func TestQueueOrderingAndChatInterval(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := register(t, s, 1, "a")
	b := register(t, s, 2, "b")
	a1 := enqueue(t, s, a, "a1")
	a2 := enqueue(t, s, a, "a2")
	b1 := enqueue(t, s, b, "b1")

	// Берётся только голова очереди каждого подписчика.
	if got := claimIDs(t, s, 0); !sameIDs(got, a1, b1) {
		t.Fatalf("first claim %v, want [%d %d]", got, a1, b1)
	}
	// Пока a1 в работе, a2 не выдаётся.
	if got := claimIDs(t, s, 0); len(got) != 0 {
		t.Fatalf("claimed %v while heads are leased", got)
	}

	if err := s.CompleteMessage(ctx, ClaimedMessage{ID: a1, SubscriberID: a.ID}, 100); err != nil {
		t.Fatal(err)
	}
	// Интервал чата ещё не прошёл.
	if got := claimIDs(t, s, time.Hour); len(got) != 0 {
		t.Fatalf("claimed %v despite chat interval", got)
	}
	if got := claimIDs(t, s, 0); !sameIDs(got, a2) {
		t.Fatalf("claim after complete %v, want [%d]", got, a2)
	}

	m, err := s.MessageByID(ctx, a1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != MessageSent || m.TelegramMessageID == nil || *m.TelegramMessageID != 100 || m.Attempts != 1 {
		t.Fatalf("sent message %+v", m)
	}
}

func TestRetryAndExpire(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := register(t, s, 1, "a")
	id := enqueue(t, s, a, "x")
	claimIDs(t, s, 0)

	// Ретрай без штрафа: попытка не засчитывается, сообщение отложено.
	if err := s.RetryMessage(ctx, id, time.Hour, "rate limited", false); err != nil {
		t.Fatal(err)
	}
	m, _ := s.MessageByID(ctx, id)
	if m.Attempts != 0 || m.Status != MessagePending || m.Error == nil {
		t.Fatalf("after retry %+v", m)
	}
	if got := claimIDs(t, s, 0); len(got) != 0 {
		t.Fatalf("claimed %v before next_attempt_at", got)
	}
	if err := s.RetryMessage(ctx, id, 0, "", true); err != nil {
		t.Fatal(err)
	}
	if got := claimIDs(t, s, 0); !sameIDs(got, id) {
		t.Fatalf("claim after retry %v", got)
	}

	old, err := s.EnqueueMessage(ctx, NewMessage{SubscriberID: a.ID, Text: "old",
		ExpiresAt: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireMessages(ctx); err != nil || n != 1 {
		t.Fatalf("expired %d, err %v", n, err)
	}
	if m, _ := s.MessageByID(ctx, old); m.Status != MessageExpired {
		t.Fatalf("status %s, want expired", m.Status)
	}
}

func TestDeactivationCancelsQueue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := register(t, s, 1, "a")
	inFlight := enqueue(t, s, a, "1")
	queued := enqueue(t, s, a, "2")
	claimIDs(t, s, 0)

	if err := s.SetStatus(ctx, a.ID, StatusBlocked, "test"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MessageByID(ctx, queued); m.Status != MessageFailed {
		t.Errorf("queued message status %s, want failed", m.Status)
	}
	// Сообщение в работе не трогаем — его завершит воркер.
	if m, _ := s.MessageByID(ctx, inFlight); m.Status != MessagePending {
		t.Errorf("in-flight message status %s, want pending", m.Status)
	}
}

func TestCallbackCodeIsOneTime(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	hash := HashCallbackCode("code")
	if _, err := s.Register(ctx, Registration{
		Profile: Profile{TelegramID: 1}, Mode: "callback", Nonce: []byte("n"),
		LinkExpiresAt: time.Now().Add(time.Hour), CallbackCodeHash: hash,
		CallbackExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	sub, nonce, err := s.RedeemCallbackCode(ctx, hash)
	if err != nil || sub.TelegramID != 1 || string(nonce) != "n" {
		t.Fatalf("redeem: %+v %q %v", sub, nonce, err)
	}
	if _, _, err := s.RedeemCallbackCode(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second redeem: got %v, want ErrNotFound", err)
	}
}

func TestListenNotifiesOnEnqueue(t *testing.T) {
	s := newTestStore(t)
	a := register(t, s, 1, "a")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notified := make(chan struct{}, 1)
	go s.ListenMessages(ctx, func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})
	time.Sleep(200 * time.Millisecond) // дать выполниться LISTEN

	enqueue(t, s, a, "x")
	select {
	case <-notified:
	case <-time.After(3 * time.Second):
		t.Fatal("no notification after enqueue")
	}
}

func sameIDs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			return false
		}
	}
	return true
}

func TestRecipients(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.Recipients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no recipients, got %v", got)
	}

	token := uuid.New()
	if _, err := s.pool.Exec(ctx, `INSERT INTO recipients (tg_id, token, alias)
		VALUES (1, $1, NULL), (2, $2, 'ops')`, token, uuid.New()); err != nil {
		t.Fatal(err)
	}

	got, err = s.Recipients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 recipients, got %v", got)
	}
	if got[0].TelegramID != 1 || got[0].Token != token || got[0].Alias != nil {
		t.Fatalf("unexpected first recipient: %+v", got[0])
	}
	if got[1].Alias == nil || *got[1].Alias != "ops" {
		t.Fatalf("unexpected second recipient: %+v", got[1])
	}
}

func TestManualRecipientIsSubscribed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	token := uuid.New()
	if _, err := s.pool.Exec(ctx, `INSERT INTO recipients (tg_id, token, alias)
		VALUES (1, $1, 'ops')`, token); err != nil {
		t.Fatal(err)
	}
	sub, err := s.SubscriberByTelegramID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != StatusActive || sub.Token != token {
		t.Fatalf("manual recipient %+v", sub)
	}

	id := enqueue(t, s, sub, "alert")
	if got := claimIDs(t, s, 0); !sameIDs(got, id) {
		t.Fatalf("claimed %v, want [%d]", got, id)
	}
	if m, err := s.MessageByID(ctx, id); err != nil || m.TelegramID != 1 {
		t.Fatalf("message %+v, err %v", m, err)
	}

	// Токен для ручного добавления необязателен.
	if _, err := s.pool.Exec(ctx, `INSERT INTO recipients (tg_id) VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	if sub, err := s.SubscriberByTelegramID(ctx, 2); err != nil || sub.Token == uuid.Nil {
		t.Fatalf("recipient without token %+v, err %v", sub, err)
	}
}

func TestRegisterKeepsManualRecipient(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	token := uuid.New()
	if _, err := s.pool.Exec(ctx, `INSERT INTO recipients (tg_id, token, alias, status)
		VALUES (1, $1, 'ops', 'unsubscribed')`, token); err != nil {
		t.Fatal(err)
	}

	sub := register(t, s, 1, "n")
	if sub.Token != token || sub.Status != StatusActive || sub.Username != "u" {
		t.Fatalf("registered %+v", sub)
	}
	got, err := s.Recipients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Alias == nil || *got[0].Alias != "ops" {
		t.Fatalf("recipients after registration: %+v", got)
	}
}

func TestAddRecipient(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, err := s.AddRecipient(ctx, Profile{TelegramID: 1, Username: "dev"}, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != StatusActive || sub.Username != "dev" || sub.Token == uuid.Nil {
		t.Fatalf("added %+v", sub)
	}

	var mode string
	if err := s.pool.QueryRow(ctx, `SELECT details->>'mode' FROM events
		WHERE subscriber_id = $1 AND kind = 'registered'`, sub.ID).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "developer" {
		t.Errorf("event mode %q", mode)
	}
}

func TestSubscribersMergeIntoRecipients(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	migrate := func(fn func(p *goose.Provider) error) {
		t.Helper()
		if err := s.WithMigrations(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	migrate(func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 3)
		return err
	})

	// Получатель 1 есть в обеих таблицах, 2 — только в subscribers, 3 — только в recipients.
	listed := uuid.New()
	if _, err := s.pool.Exec(ctx, `INSERT INTO recipients (tg_id, token, alias)
		VALUES (1, $1, 'ops'), (3, $2, NULL)`, listed, uuid.New()); err != nil {
		t.Fatal(err)
	}
	var both, only int64
	insert := `INSERT INTO subscribers (telegram_id, username, auth_token, status)
		VALUES ($1, $2, $3, $4) RETURNING id`
	// Идентификатор строки subscribers намеренно не совпадает с её будущим id в recipients.
	if err := s.pool.QueryRow(ctx, insert, 2, "two", uuid.New(), StatusBlocked).Scan(&only); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, insert, 1, "one", uuid.New(), StatusActive).Scan(&both); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO messages (subscriber_id, text, status)
		VALUES ($1, 'for one', 'sent'), ($2, 'for two', 'failed')`, both, only); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO events (subscriber_id, kind) VALUES ($1, 'registered')`, both); err != nil {
		t.Fatal(err)
	}

	migrate(func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})

	one, err := s.SubscriberByTelegramID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if one.Token != listed || one.Username != "one" {
		t.Fatalf("recipient 1 %+v", one)
	}
	two, err := s.SubscriberByTelegramID(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if two.Status != StatusBlocked || two.Username != "two" {
		t.Fatalf("recipient 2 %+v", two)
	}
	if _, err := s.SubscriberByTelegramID(ctx, 3); err != nil {
		t.Fatal(err)
	}

	var owner int64
	if err := s.pool.QueryRow(ctx, `SELECT subscriber_id FROM messages WHERE text = 'for one'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != one.ID {
		t.Errorf("message for 1 belongs to %d, want %d", owner, one.ID)
	}
	if err := s.pool.QueryRow(ctx, `SELECT subscriber_id FROM messages WHERE text = 'for two'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != two.ID {
		t.Errorf("message for 2 belongs to %d, want %d", owner, two.ID)
	}
	if err := s.pool.QueryRow(ctx, `SELECT subscriber_id FROM events`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != one.ID {
		t.Errorf("event belongs to %d, want %d", owner, one.ID)
	}

	// Откат сохраняет получателей и связанные с ними строки.
	migrate(func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 3)
		return err
	})
	var telegramID int64
	if err := s.pool.QueryRow(ctx, `SELECT s.telegram_id FROM messages m
		JOIN subscribers s ON s.id = m.subscriber_id WHERE m.text = 'for two'`).Scan(&telegramID); err != nil {
		t.Fatal(err)
	}
	if telegramID != 2 {
		t.Errorf("after rollback message for 2 belongs to telegram id %d", telegramID)
	}
	migrate(func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

func TestMigrationsDownAndUp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.WithMigrations(ctx, func(p *goose.Provider) error {
		if _, err := p.DownTo(ctx, 0); err != nil {
			return err
		}
		if version, err := p.GetDBVersion(ctx); err != nil || version != 0 {
			return fmt.Errorf("version after rollback: %d, %v", version, err)
		}
		_, err := p.Up(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recipients(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptLegacyMigrations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Состояние базы, которую вёл прежний механизм миграций.
	if err := s.WithMigrations(ctx, func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 3)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		DROP TABLE goose_db_version;
		CREATE TABLE schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO schema_migrations (version)
		VALUES ('0001_init'), ('0002_message_queue'), ('0003_recipients')`); err != nil {
		t.Fatal(err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.WithMigrations(ctx, func(p *goose.Provider) error {
		current, target, err := p.GetVersions(ctx)
		if err == nil && current != target {
			return fmt.Errorf("current version %d, want %d", current, target)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var legacy bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy {
		t.Fatal("schema_migrations must be dropped")
	}
}
