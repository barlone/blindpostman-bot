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
		VALUES (1, $1, NULL), (2, $1, 'ops')`, token); err != nil {
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
