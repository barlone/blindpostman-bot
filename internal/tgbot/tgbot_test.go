package tgbot

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

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
