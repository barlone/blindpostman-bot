package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/blind-postman/blind-postman/internal/storage"
	"github.com/blind-postman/blind-postman/pkg/startlink"
)

const (
	testAPIKey = "test-api-key-0123456789"
	testAlias  = "ops" // псевдоним подписчика 100 в fakeStore
	testGroup  = "oncall"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

type fakeStore struct {
	subs     map[int64]*storage.Subscriber
	messages []storage.NewMessage
}

func newFakeStore(subs ...storage.Subscriber) *fakeStore {
	f := &fakeStore{subs: map[int64]*storage.Subscriber{}}
	for i := range subs {
		f.subs[subs[i].TelegramID] = &subs[i]
	}
	return f
}

func (f *fakeStore) Ping(context.Context) error { return nil }

func (f *fakeStore) SubscriberByTelegramID(_ context.Context, id int64) (storage.Subscriber, error) {
	if s, ok := f.subs[id]; ok {
		return *s, nil
	}
	return storage.Subscriber{}, storage.ErrNotFound
}

func (f *fakeStore) SubscriberByToken(_ context.Context, t uuid.UUID) (storage.Subscriber, error) {
	for _, s := range f.subs {
		if s.Token == t {
			return *s, nil
		}
	}
	return storage.Subscriber{}, storage.ErrNotFound
}

func (f *fakeStore) SubscriberByAlias(ctx context.Context, alias string) (storage.Subscriber, error) {
	if alias != testAlias {
		return storage.Subscriber{}, storage.ErrNotFound
	}
	return f.SubscriberByTelegramID(ctx, 100)
}

func (f *fakeStore) RedeemCallbackCode(context.Context, []byte) (storage.Subscriber, []byte, error) {
	return storage.Subscriber{}, nil, storage.ErrNotFound
}

func (f *fakeStore) EnqueueMessage(_ context.Context, m storage.NewMessage) (int64, error) {
	f.messages = append(f.messages, m)
	return int64(len(f.messages)), nil
}

// EnqueueGroupMessage ведёт себя как группа testGroup из двух активных участников и группа
// "nobody" без активных; состав групп проверяют тесты storage.
func (f *fakeStore) EnqueueGroupMessage(ctx context.Context, alias string, m storage.NewMessage) ([]int64, error) {
	switch alias {
	case testGroup:
		first, _ := f.EnqueueMessage(ctx, m)
		second, _ := f.EnqueueMessage(ctx, m)
		return []int64{first, second}, nil
	case "nobody":
		return nil, storage.ErrGroupEmpty
	}
	return nil, storage.ErrNotFound
}

func (f *fakeStore) MessageByID(_ context.Context, id int64) (storage.Message, error) {
	if id < 1 || id > int64(len(f.messages)) {
		return storage.Message{}, storage.ErrNotFound
	}
	return storage.Message{ID: id, Status: storage.MessagePending, Attempts: 0}, nil
}

type fakeBot struct{}

func (fakeBot) Username() string { return "test_bot" }

func newTestServer(store Store) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, fakeBot{}, Config{
		Secret:     testSecret,
		APIKey:     testAPIKey,
		LinkTTL:    time.Hour,
		MessageTTL: time.Hour,
	}, log).Handler()
}

func do(t *testing.T, h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func activeSub() storage.Subscriber {
	return storage.Subscriber{ID: 1, TelegramID: 100, Token: uuid.New(), Status: storage.StatusActive}
}

func TestAuth(t *testing.T) {
	h := newTestServer(newFakeStore())
	for _, key := range []string{"", "wrong"} {
		if rec := do(t, h, "GET", "/api/v1/subscribers/100", "", key); rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: got %d, want 401", key, rec.Code)
		}
	}
}

func TestSendMessage(t *testing.T) {
	sub := activeSub()
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"by alias", `{"alias":"ops","text":"disk full"}`, http.StatusAccepted},
		{"by token", `{"token":"` + sub.Token.String() + `","text":"disk full"}`, http.StatusAccepted},
		{"token and alias", `{"token":"` + sub.Token.String() + `","alias":"ops","text":"x"}`, http.StatusBadRequest},
		{"no recipient", `{"text":"x"}`, http.StatusBadRequest},
		{"telegram id is not accepted", `{"telegram_id":100,"text":"x"}`, http.StatusBadRequest},
		{"bad token", `{"token":"nope","text":"x"}`, http.StatusBadRequest},
		{"unknown token", `{"token":"` + uuid.NewString() + `","text":"x"}`, http.StatusNotFound},
		{"unknown alias", `{"alias":"absent","text":"x"}`, http.StatusNotFound},
		{"empty text", `{"alias":"ops","text":" "}`, http.StatusBadRequest},
		{"bad parse mode", `{"alias":"ops","text":"x","parse_mode":"bbcode"}`, http.StatusBadRequest},
		{"negative ttl", `{"alias":"ops","text":"x","ttl_seconds":-1}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore(sub)
			rec := do(t, newTestServer(store), "POST", "/api/v1/messages", tt.body, testAPIKey)
			if rec.Code != tt.wantCode {
				t.Fatalf("got %d (%s), want %d", rec.Code, rec.Body, tt.wantCode)
			}
			queued := tt.wantCode == http.StatusAccepted
			if got := len(store.messages) == 1; got != queued {
				t.Fatalf("message queued = %v, want %v", got, queued)
			}
			if queued && store.messages[0].SubscriberID != sub.ID {
				t.Fatalf("message for subscriber %d, want %d", store.messages[0].SubscriberID, sub.ID)
			}
		})
	}
}

func TestSendToGroup(t *testing.T) {
	sub := activeSub()
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"known group", `{"group_alias":"oncall","text":"disk full"}`, http.StatusAccepted},
		{"unknown group", `{"group_alias":"absent","text":"x"}`, http.StatusNotFound},
		{"no active members", `{"group_alias":"nobody","text":"x"}`, http.StatusConflict},
		{"group and alias", `{"group_alias":"oncall","alias":"ops","text":"x"}`, http.StatusBadRequest},
		{"group and token", `{"group_alias":"oncall","token":"` + sub.Token.String() + `","text":"x"}`, http.StatusBadRequest},
		{"empty text", `{"group_alias":"oncall","text":" "}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore(sub)
			rec := do(t, newTestServer(store), "POST", "/api/v1/messages", tt.body, testAPIKey)
			if rec.Code != tt.wantCode {
				t.Fatalf("got %d (%s), want %d", rec.Code, rec.Body, tt.wantCode)
			}
			if tt.wantCode != http.StatusAccepted {
				if len(store.messages) != 0 {
					t.Fatalf("%d messages queued, want none", len(store.messages))
				}
				return
			}

			var resp sendMessageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.MessageIDs) != 2 || resp.MessageID != 0 || resp.Status != storage.MessagePending || resp.ExpiresAt == nil {
				t.Errorf("unexpected response %s", rec.Body)
			}
			if rec.Header().Get("Location") != "" {
				t.Errorf("Location = %q, want none", rec.Header().Get("Location"))
			}
		})
	}
}

func TestSendMessageTTL(t *testing.T) {
	store := newFakeStore(activeSub())
	h := newTestServer(store)
	before := time.Now()

	rec := do(t, h, "POST", "/api/v1/messages", `{"alias":"ops","text":"x","ttl_seconds":60}`, testAPIKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Location") != "/api/v1/messages/1" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	exp := store.messages[0].ExpiresAt
	if exp.Before(before.Add(59*time.Second)) || exp.After(time.Now().Add(61*time.Second)) {
		t.Errorf("expires_at %v is not ~60s from now", exp)
	}
}

func TestSendToUnsubscribed(t *testing.T) {
	sub := activeSub()
	sub.Status = storage.StatusUnsubscribed
	h := newTestServer(newFakeStore(sub))
	if rec := do(t, h, "POST", "/api/v1/messages", `{"alias":"ops","text":"x"}`, testAPIKey); rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rec.Code)
	}
}

func TestGetMessage(t *testing.T) {
	store := newFakeStore(activeSub())
	h := newTestServer(store)
	do(t, h, "POST", "/api/v1/messages", `{"alias":"ops","text":"x"}`, testAPIKey)

	if rec := do(t, h, "GET", "/api/v1/messages/1", "", testAPIKey); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/v1/messages/2", "", testAPIKey); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestCreateLink(t *testing.T) {
	h := newTestServer(newFakeStore())
	rec := do(t, h, "POST", "/api/v1/registration-links", `{"mode":"callback","ttl_seconds":600}`, testAPIKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var resp createLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Link, "https://t.me/test_bot?start=") {
		t.Errorf("unexpected link %q", resp.Link)
	}
	p, err := startlink.Decode(testSecret, resp.StartParam, time.Now())
	if err != nil {
		t.Fatalf("decode issued param: %v", err)
	}
	if p.Mode != startlink.ModeCallback || p.NonceHex() != resp.Nonce {
		t.Errorf("payload %+v does not match response %+v", p, resp)
	}
}
