package tgbot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/google/uuid"

	"github.com/blind-postman/blind-postman/internal/storage"
)

var testToken = uuid.MustParse("11111111-2222-3333-4444-555555555555")

// newTestService создаёт сервис, который вместо Telegram обращается к тестовому серверу.
// В возвращаемый канал попадают поля запросов sendMessage.
func newTestService(t *testing.T) (*Service, <-chan map[string]string) {
	t.Helper()
	sent := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse request: %v", err)
		}
		fields := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		sent <- fields
		io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":100,"type":"private"}}}`)
	}))
	t.Cleanup(srv.Close)

	b, err := bot.New("test:token", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{bot: b, log: slog.New(slog.NewTextHandler(io.Discard, nil))}, sent
}

func commandUpdate(chatType models.ChatType, text string) *models.Update {
	return &models.Update{Message: &models.Message{
		Text: text,
		From: &models.User{ID: 123456789},
		Chat: models.Chat{ID: 100, Type: chatType},
	}}
}

type fakeStore struct {
	Store
	subs  map[int64]storage.Subscriber
	added []storage.Profile
}

func (f *fakeStore) SubscriberByTelegramID(_ context.Context, id int64) (storage.Subscriber, error) {
	if sub, ok := f.subs[id]; ok {
		return sub, nil
	}
	return storage.Subscriber{}, storage.ErrNotFound
}

func (f *fakeStore) AddRecipient(_ context.Context, p storage.Profile, mode string) (storage.Subscriber, error) {
	if mode != "developer" {
		return storage.Subscriber{}, fmt.Errorf("unexpected mode %q", mode)
	}
	f.added = append(f.added, p)
	return storage.Subscriber{TelegramID: p.TelegramID, Token: testToken}, nil
}

func sentText(t *testing.T, sent <-chan map[string]string) string {
	t.Helper()
	select {
	case got := <-sent:
		return got["text"]
	default:
		t.Fatal("no message sent")
		return ""
	}
}

func TestStartWithoutLink(t *testing.T) {
	const developer, stranger, listed = 123456789, 987654321, 555

	tests := []struct {
		name  string
		id    int64
		text  string // часть ответа
		added bool
	}{
		{"developer is added", developer, testToken.String(), true},
		{"stranger is not added", stranger, "откройте ссылку регистрации", false},
		{"recipient stays as is", listed, "уже подписаны", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, sent := newTestService(t)
			store := &fakeStore{subs: map[int64]storage.Subscriber{
				listed: {TelegramID: listed, Status: storage.StatusActive},
			}}
			s.store, s.developerID = store, developer

			u := commandUpdate(models.ChatTypePrivate, "/start")
			u.Message.From = &models.User{ID: tt.id, Username: "user"}
			s.onStart(context.Background(), nil, u)

			if got := sentText(t, sent); !strings.Contains(got, tt.text) {
				t.Errorf("reply %q, want %q", got, tt.text)
			}
			if tt.added && (len(store.added) != 1 || store.added[0].TelegramID != tt.id || store.added[0].Username != "user") {
				t.Errorf("added %+v", store.added)
			}
			if !tt.added && len(store.added) != 0 {
				t.Errorf("unexpected recipient %+v", store.added)
			}
		})
	}
}

func TestStartWithoutDeveloperConfigured(t *testing.T) {
	s, sent := newTestService(t)
	store := &fakeStore{}
	s.store = store

	s.onStart(context.Background(), nil, commandUpdate(models.ChatTypePrivate, "/start"))

	if got := sentText(t, sent); got != textWelcome {
		t.Errorf("reply %q", got)
	}
	if len(store.added) != 0 {
		t.Errorf("unexpected recipient %+v", store.added)
	}
}

func TestMyID(t *testing.T) {
	s, sent := newTestService(t)
	s.onMyID(context.Background(), nil, commandUpdate(models.ChatTypePrivate, "/myid"))

	select {
	case got := <-sent:
		if want := "Ваш Telegram ID: <code>123456789</code>"; got["text"] != want {
			t.Errorf("text %q, want %q", got["text"], want)
		}
		if got["chat_id"] != "100" || got["parse_mode"] != "HTML" {
			t.Errorf("unexpected request: %v", got)
		}
	default:
		t.Fatal("no message sent")
	}
}

func TestMyIDIgnoresGroups(t *testing.T) {
	s, sent := newTestService(t)
	s.onMyID(context.Background(), nil, commandUpdate(models.ChatTypeGroup, "/myid"))

	select {
	case got := <-sent:
		t.Fatalf("unexpected message: %v", got)
	default:
	}
}
