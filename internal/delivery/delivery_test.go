package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/blind-postman/blind-postman/internal/storage"
	"github.com/blind-postman/blind-postman/internal/tgbot"
)

type outcome struct {
	status       string // sent | failed | expired | retry
	delay        time.Duration
	countAttempt bool
}

type fakeStore struct {
	mu        sync.Mutex
	queue     []storage.ClaimedMessage
	outcomes  map[int64]outcome
	subStatus map[int64]string
	expires   int
	purges    []int64 // сколько строк вернёт каждый вызов PurgeMessages
	purged    []time.Duration
}

func newFakeStore(msgs ...storage.ClaimedMessage) *fakeStore {
	return &fakeStore{queue: msgs, outcomes: map[int64]outcome{}, subStatus: map[int64]string{}}
}

func (f *fakeStore) ClaimMessages(_ context.Context, limit int, _, _ time.Duration) ([]storage.ClaimedMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := min(limit, len(f.queue))
	out := f.queue[:n]
	f.queue = f.queue[n:]
	return out, nil
}

func (f *fakeStore) set(id int64, o outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes[id] = o
}

func (f *fakeStore) get(id int64) outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outcomes[id]
}

func (f *fakeStore) CompleteMessage(_ context.Context, m storage.ClaimedMessage, _ int) error {
	f.set(m.ID, outcome{status: storage.MessageSent})
	return nil
}

func (f *fakeStore) FinishMessage(_ context.Context, id int64, status, _ string) error {
	f.set(id, outcome{status: status})
	return nil
}

func (f *fakeStore) RetryMessage(_ context.Context, id int64, delay time.Duration, _ string, count bool) error {
	f.set(id, outcome{status: "retry", delay: delay, countAttempt: count})
	return nil
}

func (f *fakeStore) ExpireMessages(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expires++
	return 0, nil
}

func (f *fakeStore) PurgeMessages(_ context.Context, olderThan time.Duration, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = append(f.purged, olderThan)
	if len(f.purges) == 0 {
		return 0, nil
	}
	n := f.purges[0]
	f.purges = f.purges[1:]
	return n, nil
}

func (f *fakeStore) SetStatus(_ context.Context, id int64, status, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subStatus[id] = status
	return nil
}

func (f *fakeStore) ListenMessages(ctx context.Context, _ func()) error {
	<-ctx.Done()
	return ctx.Err()
}

type fakeSender struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (s *fakeSender) Send(context.Context, int64, string, tgbot.SendOptions) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return 7, s.err
}

func newDispatcher(store Store, sender Sender) *Dispatcher {
	return New(Config{MaxAttempts: 3, GlobalRate: 1000, PollInterval: 10 * time.Millisecond},
		store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func msg(attempt int) storage.ClaimedMessage {
	return storage.ClaimedMessage{ID: 1, SubscriberID: 10, TelegramID: 100,
		SubscriberStatus: storage.StatusActive, Text: "alert", Attempts: attempt}
}

func TestDeliverOutcomes(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	expired := msg(1)
	expired.ExpiresAt = &past
	inactive := msg(1)
	inactive.SubscriberStatus = storage.StatusUnsubscribed

	tests := []struct {
		name       string
		msg        storage.ClaimedMessage
		sendErr    error
		want       outcome
		wantSend   bool
		wantStatus string // новый статус подписчика
	}{
		{"sent", msg(1), nil, outcome{status: storage.MessageSent}, true, ""},
		{"blocked by user", msg(1), tgbot.ErrForbidden, outcome{status: storage.MessageFailed}, true, storage.StatusBlocked},
		{"rejected", msg(1), tgbot.ErrRejected, outcome{status: storage.MessageFailed}, true, ""},
		{"transient", msg(1), errors.New("connection reset"), outcome{status: "retry", countAttempt: true}, true, ""},
		{"max attempts", msg(3), errors.New("connection reset"), outcome{status: storage.MessageFailed}, true, ""},
		{"rate limited", msg(1), &tgbot.RateLimitError{RetryAfter: 5 * time.Second},
			outcome{status: "retry", delay: 5 * time.Second, countAttempt: false}, true, ""},
		{"expired", expired, nil, outcome{status: storage.MessageExpired}, false, ""},
		{"inactive subscriber", inactive, nil, outcome{status: storage.MessageFailed}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			sender := &fakeSender{err: tt.sendErr}
			d := newDispatcher(store, sender)
			d.deliver(context.Background(), tt.msg)

			got := store.get(tt.msg.ID)
			if tt.want.status == "retry" && tt.want.delay == 0 {
				got.delay = 0 // задержка бэкоффа случайна, проверяется отдельно
			}
			if got != tt.want {
				t.Errorf("outcome %+v, want %+v", got, tt.want)
			}
			if (sender.calls > 0) != tt.wantSend {
				t.Errorf("send called %d times, want send=%v", sender.calls, tt.wantSend)
			}
			if st := store.subStatus[tt.msg.SubscriberID]; st != tt.wantStatus {
				t.Errorf("subscriber status %q, want %q", st, tt.wantStatus)
			}
		})
	}
}

func TestRateLimitPausesDelivery(t *testing.T) {
	store := newFakeStore()
	sender := &fakeSender{err: &tgbot.RateLimitError{RetryAfter: time.Minute}}
	d := newDispatcher(store, sender)
	d.deliver(context.Background(), msg(1))

	if d.pauseLeft() < 59*time.Second {
		t.Fatalf("pause = %v, want ~1m", d.pauseLeft())
	}
	// Пока действует пауза, следующее сообщение не отправляется, а возвращается без штрафа.
	next := msg(1)
	next.ID = 2
	d.deliver(context.Background(), next)
	if sender.calls != 1 {
		t.Fatalf("send called %d times during pause, want 1", sender.calls)
	}
	if o := store.get(2); o.status != "retry" || o.countAttempt {
		t.Fatalf("outcome during pause %+v", o)
	}
}

func TestRunDeliversQueue(t *testing.T) {
	var msgs []storage.ClaimedMessage
	for i := range 20 {
		m := msg(1)
		m.ID = int64(i + 1)
		msgs = append(msgs, m)
	}
	store := newFakeStore(msgs...)
	sender := &fakeSender{}
	d := newDispatcher(store, sender)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	deadline := time.After(5 * time.Second)
	for {
		sender.mu.Lock()
		n := sender.calls
		sender.mu.Unlock()
		if n == len(msgs) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("delivered %d of %d", n, len(msgs))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	for _, m := range msgs {
		if o := store.get(m.ID); o.status != storage.MessageSent {
			t.Errorf("message %d: %+v", m.ID, o)
		}
	}
}

func TestExpireDoesNotRunOnEveryClaim(t *testing.T) {
	var msgs []storage.ClaimedMessage
	for i := range 20 {
		m := msg(1)
		m.ID = int64(i + 1)
		msgs = append(msgs, m)
	}
	store := newFakeStore(msgs...)
	sender := &fakeSender{}
	d := New(Config{Workers: 1, GlobalRate: 1000, PollInterval: time.Millisecond, ExpireInterval: time.Hour},
		store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	deadline := time.After(5 * time.Second)
	for {
		sender.mu.Lock()
		n := sender.calls
		sender.mu.Unlock()
		if n == len(msgs) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("delivered %d of %d", n, len(msgs))
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.expires > 1 {
		t.Fatalf("expire ran %d times, want only the start-up run", store.expires)
	}
}

func TestPurge(t *testing.T) {
	newD := func(store Store, retention time.Duration) *Dispatcher {
		return New(Config{Retention: retention}, store, &fakeSender{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Без срока хранения ничего не удаляется.
	store := newFakeStore()
	newD(store, 0).purge(context.Background())
	if len(store.purged) != 0 {
		t.Fatalf("purge ran with retention disabled: %v", store.purged)
	}

	// Полная пачка значит, что осталось ещё; неполная завершает обход.
	store = newFakeStore()
	store.purges = []int64{purgeBatch, purgeBatch, 3, purgeBatch}
	newD(store, time.Hour).purge(context.Background())
	if len(store.purged) != 3 {
		t.Fatalf("purge called %d times, want 3", len(store.purged))
	}
	if store.purged[0] != time.Hour {
		t.Fatalf("purge older than %v, want 1h", store.purged[0])
	}
}

func TestBackoff(t *testing.T) {
	base, maxDelay := 2*time.Second, time.Minute
	for attempt, want := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 4: 16 * time.Second, 10: time.Minute, 100: time.Minute} {
		got := backoff(attempt, base, maxDelay)
		if got < want*8/10 || got > want*12/10 {
			t.Errorf("attempt %d: %v, want %v±20%%", attempt, got, want)
		}
	}
}
