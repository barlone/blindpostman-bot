// Package tgbot — Telegram-часть: регистрация подписчиков по /start и отправка сообщений.
package tgbot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/blind-postman/blind-postman/internal/storage"
	"github.com/blind-postman/blind-postman/pkg/startlink"
)

var (
	// ErrForbidden — пользователь заблокировал бота или удалил чат.
	ErrForbidden = errors.New("tgbot: recipient blocked the bot")
	// ErrRejected — Telegram отверг сообщение (400): повтор не поможет.
	ErrRejected = errors.New("tgbot: message rejected")
)

// RateLimitError — Telegram ответил 429.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("tgbot: rate limited, retry after %s", e.RetryAfter)
}

// Store — часть хранилища, нужная боту.
type Store interface {
	Register(ctx context.Context, r storage.Registration) (storage.Subscriber, error)
	AddRecipient(ctx context.Context, p storage.Profile, mode string) (storage.Subscriber, error)
	SubscriberByTelegramID(ctx context.Context, telegramID int64) (storage.Subscriber, error)
	SetStatus(ctx context.Context, subscriberID int64, status, reason string) error
}

type Config struct {
	Token       string
	DeveloperID int64 // 0 — пользователя-исключения нет
	Secret      []byte
	CallbackURL string // пустой — режим callback отключён
	CallbackTTL time.Duration
}

type Service struct {
	bot         *bot.Bot
	store       Store
	developerID int64
	secret      []byte
	callbackURL *url.URL
	callbackTTL time.Duration
	username    string
	log         *slog.Logger
}

func New(ctx context.Context, cfg Config, store Store, log *slog.Logger) (*Service, error) {
	s := &Service{
		store:       store,
		developerID: cfg.DeveloperID,
		secret:      cfg.Secret,
		callbackTTL: cfg.CallbackTTL,
		log:         log.With("component", "tgbot"),
	}
	if cfg.CallbackURL != "" {
		u, err := url.Parse(cfg.CallbackURL)
		if err != nil {
			return nil, fmt.Errorf("tgbot: callback url: %w", err)
		}
		s.callbackURL = u
	}

	b, err := bot.New(cfg.Token,
		bot.WithDefaultHandler(s.onOther),
		bot.WithErrorsHandler(func(err error) { s.log.Error("telegram error", "err", err) }),
		bot.WithMessageTextHandler("start", bot.MatchTypeCommandStartOnly, s.onStart),
		bot.WithMessageTextHandler("stop", bot.MatchTypeCommandStartOnly, s.onStop),
		bot.WithMessageTextHandler("status", bot.MatchTypeCommandStartOnly, s.onStatus),
		bot.WithMessageTextHandler("myid", bot.MatchTypeCommandStartOnly, s.onMyID),
		bot.WithMessageTextHandler("help", bot.MatchTypeCommandStartOnly, s.onHelp),
	)
	if err != nil {
		return nil, fmt.Errorf("tgbot: init: %w", err)
	}
	s.bot = b

	me, err := b.GetMe(ctx)
	if err != nil {
		return nil, fmt.Errorf("tgbot: getMe: %w", err)
	}
	s.username = me.Username

	if _, err := b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: []models.BotCommand{
		{Command: "status", Description: "Статус подписки"},
		{Command: "stop", Description: "Отписаться от уведомлений"},
		{Command: "myid", Description: "Мой Telegram ID"},
		{Command: "help", Description: "Справка"},
	}}); err != nil {
		s.log.Warn("set commands failed", "err", err)
	}
	return s, nil
}

// Username — имя бота без @, нужно для построения deep link.
func (s *Service) Username() string { return s.username }

// Run запускает long polling и блокируется до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	s.log.Info("bot started", "username", s.username)
	s.bot.Start(ctx)
}

type SendOptions struct {
	ParseMode           string
	DisableNotification bool
}

// Send отправляет сообщение в личный чат пользователя и возвращает ID сообщения в Telegram.
func (s *Service) Send(ctx context.Context, telegramID int64, text string, o SendOptions) (int, error) {
	m, err := s.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              telegramID,
		Text:                text,
		ParseMode:           models.ParseMode(o.ParseMode),
		DisableNotification: o.DisableNotification,
		LinkPreviewOptions:  &models.LinkPreviewOptions{IsDisabled: bot.True()},
	})
	if err != nil {
		var tooMany *bot.TooManyRequestsError
		switch {
		case errors.Is(err, bot.ErrorForbidden):
			return 0, fmt.Errorf("%w: %v", ErrForbidden, err)
		case errors.Is(err, bot.ErrorBadRequest):
			return 0, fmt.Errorf("%w: %v", ErrRejected, err)
		case errors.As(err, &tooMany):
			return 0, &RateLimitError{RetryAfter: time.Duration(tooMany.RetryAfter) * time.Second}
		}
		return 0, err
	}
	return m.ID, nil
}

const (
	textWelcome = "Это бот уведомлений.\n\n" +
		"Для подписки откройте ссылку регистрации, полученную в сервисе."
	textHelp = "/status — статус подписки\n" +
		"/stop — отписаться от уведомлений\n" +
		"/start — возобновить подписку\n" +
		"/myid — мой Telegram ID\n\n" +
		"Для новой регистрации используйте ссылку из сервиса."
	textPrivateOnly = "Бот работает только в личных сообщениях."
	textInternal    = "Внутренняя ошибка, попробуйте позже."
)

func (s *Service) onStart(ctx context.Context, _ *bot.Bot, u *models.Update) {
	msg, ok := privateMessage(u)
	if !ok {
		s.replyIfChat(ctx, u, textPrivateOnly)
		return
	}
	log := s.log.With("telegram_id", msg.From.ID)

	args := strings.Fields(msg.Text)
	if len(args) < 2 {
		s.resume(ctx, msg)
		return
	}

	p, err := startlink.Decode(s.secret, args[1], time.Now())
	switch {
	case errors.Is(err, startlink.ErrExpired):
		s.reply(ctx, msg.Chat.ID, "Срок действия ссылки истёк. Запросите новую ссылку в сервисе.")
		return
	case err != nil:
		log.Warn("invalid start payload", "err", err)
		s.reply(ctx, msg.Chat.ID, "Ссылка регистрации недействительна.")
		return
	}

	reg := storage.Registration{
		Profile: storage.Profile{
			TelegramID: msg.From.ID,
			Username:   msg.From.Username,
			FirstName:  msg.From.FirstName,
			LastName:   msg.From.LastName,
		},
		Mode:          p.Mode.String(),
		Nonce:         p.Nonce[:],
		LinkExpiresAt: p.ExpiresAt,
	}

	var code string
	if p.Mode == startlink.ModeCallback {
		if s.callbackURL == nil {
			log.Error("callback registration requested but callback_url is not configured")
			s.reply(ctx, msg.Chat.ID, "Регистрация по этой ссылке сейчас недоступна.")
			return
		}
		if code, err = newCallbackCode(); err != nil {
			log.Error("generate callback code", "err", err)
			s.reply(ctx, msg.Chat.ID, textInternal)
			return
		}
		reg.CallbackCodeHash = storage.HashCallbackCode(code)
		reg.CallbackExpiresAt = time.Now().Add(s.callbackTTL)
	}

	sub, err := s.store.Register(ctx, reg)
	switch {
	case errors.Is(err, storage.ErrLinkUsed):
		s.reply(ctx, msg.Chat.ID, "Эта ссылка уже использована. Запросите новую ссылку в сервисе.")
		return
	case err != nil:
		log.Error("register subscriber", "err", err)
		s.reply(ctx, msg.Chat.ID, textInternal)
		return
	}
	log.Info("subscriber registered", "mode", p.Mode.String(), "nonce", p.NonceHex())

	switch p.Mode {
	case startlink.ModeToken:
		s.replyToken(ctx, msg.Chat.ID, sub)
	case startlink.ModeCallback:
		link := s.callbackLink(code)
		s.replyHTML(ctx, msg.Chat.ID, fmt.Sprintf(
			"Вы подписаны на уведомления.\n\n"+
				"Чтобы завершить регистрацию, перейдите по одноразовой ссылке (действует %s):\n%s",
			s.callbackTTL, html.EscapeString(link)))
	}
}

func (s *Service) replyToken(ctx context.Context, chatID int64, sub storage.Subscriber) {
	s.replyHTML(ctx, chatID, fmt.Sprintf(
		"Вы подписаны на уведомления.\n\nВаш токен авторизации:\n<code>%s</code>\n\n"+
			"Скопируйте его и укажите в сервисе.", sub.Token))
}

// resume обрабатывает /start без параметра: реактивирует существующую подписку,
// а разработчика добавляет в список получателей.
func (s *Service) resume(ctx context.Context, msg *models.Message) {
	sub, err := s.store.SubscriberByTelegramID(ctx, msg.From.ID)
	if errors.Is(err, storage.ErrNotFound) {
		if msg.From.ID == s.developerID {
			s.addDeveloper(ctx, msg)
			return
		}
		s.reply(ctx, msg.Chat.ID, textWelcome)
		return
	}
	if err != nil {
		s.log.Error("load subscriber", "err", err, "telegram_id", msg.From.ID)
		s.reply(ctx, msg.Chat.ID, textInternal)
		return
	}
	if sub.Status == storage.StatusActive {
		s.reply(ctx, msg.Chat.ID, "Вы уже подписаны на уведомления.")
		return
	}
	if err := s.store.SetStatus(ctx, sub.ID, storage.StatusActive, "user /start"); err != nil {
		s.log.Error("resume subscription", "err", err, "telegram_id", msg.From.ID)
		s.reply(ctx, msg.Chat.ID, textInternal)
		return
	}
	s.reply(ctx, msg.Chat.ID, "Подписка возобновлена.")
}

func (s *Service) addDeveloper(ctx context.Context, msg *models.Message) {
	sub, err := s.store.AddRecipient(ctx, storage.Profile{
		TelegramID: msg.From.ID,
		Username:   msg.From.Username,
		FirstName:  msg.From.FirstName,
		LastName:   msg.From.LastName,
	}, "developer")
	if err != nil {
		s.log.Error("add developer", "err", err, "telegram_id", msg.From.ID)
		s.reply(ctx, msg.Chat.ID, textInternal)
		return
	}
	s.log.Info("subscriber registered", "mode", "developer", "telegram_id", msg.From.ID)
	s.replyToken(ctx, msg.Chat.ID, sub)
}

func (s *Service) onStop(ctx context.Context, _ *bot.Bot, u *models.Update) {
	msg, ok := privateMessage(u)
	if !ok {
		return
	}
	sub, err := s.store.SubscriberByTelegramID(ctx, msg.From.ID)
	if errors.Is(err, storage.ErrNotFound) {
		s.reply(ctx, msg.Chat.ID, "Вы не подписаны.")
		return
	}
	if err == nil {
		err = s.store.SetStatus(ctx, sub.ID, storage.StatusUnsubscribed, "user /stop")
	}
	if err != nil {
		s.log.Error("unsubscribe", "err", err, "telegram_id", msg.From.ID)
		s.reply(ctx, msg.Chat.ID, textInternal)
		return
	}
	s.reply(ctx, msg.Chat.ID, "Вы отписаны от уведомлений. Чтобы возобновить — отправьте /start.")
}

func (s *Service) onStatus(ctx context.Context, _ *bot.Bot, u *models.Update) {
	msg, ok := privateMessage(u)
	if !ok {
		return
	}
	sub, err := s.store.SubscriberByTelegramID(ctx, msg.From.ID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		s.reply(ctx, msg.Chat.ID, "Вы не подписаны.")
	case err != nil:
		s.log.Error("load subscriber", "err", err, "telegram_id", msg.From.ID)
		s.reply(ctx, msg.Chat.ID, textInternal)
	default:
		s.reply(ctx, msg.Chat.ID, fmt.Sprintf("Статус подписки: %s\nЗарегистрирован: %s",
			sub.Status, sub.CreatedAt.Format("2006-01-02 15:04 MST")))
	}
}

func (s *Service) onMyID(ctx context.Context, _ *bot.Bot, u *models.Update) {
	if msg, ok := privateMessage(u); ok {
		s.replyHTML(ctx, msg.Chat.ID, fmt.Sprintf("Ваш Telegram ID: <code>%d</code>", msg.From.ID))
	}
}

func (s *Service) onHelp(ctx context.Context, _ *bot.Bot, u *models.Update) {
	if msg, ok := privateMessage(u); ok {
		s.reply(ctx, msg.Chat.ID, textHelp)
	}
}

func (s *Service) onOther(ctx context.Context, b *bot.Bot, u *models.Update) {
	s.onHelp(ctx, b, u)
}

func (s *Service) callbackLink(code string) string {
	u := *s.callbackURL
	q := u.Query()
	q.Set("code", code)
	u.RawQuery = q.Encode()
	return u.String()
}

func newCallbackCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func privateMessage(u *models.Update) (*models.Message, bool) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != models.ChatTypePrivate {
		return nil, false
	}
	return m, true
}

func (s *Service) replyIfChat(ctx context.Context, u *models.Update, text string) {
	if u.Message != nil {
		s.reply(ctx, u.Message.Chat.ID, text)
	}
}

func (s *Service) reply(ctx context.Context, chatID int64, text string) {
	s.send(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
}

func (s *Service) replyHTML(ctx context.Context, chatID int64, text string) {
	s.send(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()}})
}

func (s *Service) send(ctx context.Context, p *bot.SendMessageParams) {
	if _, err := s.bot.SendMessage(ctx, p); err != nil {
		s.log.Error("reply failed", "err", err, "chat_id", p.ChatID)
	}
}
