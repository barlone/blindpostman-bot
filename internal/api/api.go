// Package api — REST API для регистрирующей службы: выдача ссылок регистрации,
// погашение callback-кодов и отправка сообщений подписчикам.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/blind-postman/blind-postman/internal/docs"
	"github.com/blind-postman/blind-postman/internal/storage"
	"github.com/blind-postman/blind-postman/pkg/startlink"
)

const (
	maxBodySize    = 64 << 10
	maxMessageLen  = 4096 // лимит Telegram на длину текста сообщения
	maxLinkTTL     = 30 * 24 * time.Hour
	requestTimeout = 30 * time.Second
)

type Store interface {
	Ping(ctx context.Context) error
	SubscriberByTelegramID(ctx context.Context, telegramID int64) (storage.Subscriber, error)
	SubscriberByToken(ctx context.Context, token uuid.UUID) (storage.Subscriber, error)
	SubscriberByAlias(ctx context.Context, alias string) (storage.Subscriber, error)
	RedeemCallbackCode(ctx context.Context, codeHash []byte) (storage.Subscriber, []byte, error)
	EnqueueMessage(ctx context.Context, m storage.NewMessage) (int64, error)
	MessageByID(ctx context.Context, id int64) (storage.Message, error)
}

type Bot interface {
	Username() string
}

type Config struct {
	Secret  []byte
	APIKey  string
	LinkTTL time.Duration
	// MessageTTL — срок актуальности сообщения по умолчанию; 0 — бессрочно.
	MessageTTL time.Duration
}

type Server struct {
	store      Store
	bot        Bot
	secret     []byte
	apiKeyHash [sha256.Size]byte
	linkTTL    time.Duration
	messageTTL time.Duration
	log        *slog.Logger
}

func New(store Store, bot Bot, cfg Config, log *slog.Logger) *Server {
	return &Server{
		store:      store,
		bot:        bot,
		secret:     cfg.Secret,
		apiKeyHash: sha256.Sum256([]byte(cfg.APIKey)),
		linkTTL:    cfg.LinkTTL,
		messageTTL: cfg.MessageTTL,
		log:        log.With("component", "api"),
	}
}

type route struct {
	pattern string
	handler http.Handler
}

// routes перечисляет методы API. Каждый из них описан в internal/docs/openapi.yaml.
func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", http.HandlerFunc(s.health)},
		{"POST /api/v1/messages", s.auth(s.sendMessage)},
		{"GET /api/v1/messages/{id}", s.auth(s.getMessage)},
		{"POST /api/v1/registration-links", s.auth(s.createLink)},
		{"POST /api/v1/registrations/redeem", s.auth(s.redeem)},
		{"POST /api/v1/subscribers/resolve", s.auth(s.resolveToken)},
		{"GET /api/v1/subscribers/{telegram_id}", s.auth(s.getSubscriber)},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.Handle(r.pattern, r.handler)
	}
	return s.logRequests(docs.Wrap(http.TimeoutHandler(mux, requestTimeout, `{"error":"timeout"}`)))
}

// --- handlers ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type sendMessageRequest struct {
	// Token и Alias задают получателя: нужно ровно одно из этих полей.
	Token               string `json:"token,omitempty"`
	Alias               string `json:"alias,omitempty"`
	Text                string `json:"text"`
	ParseMode           string `json:"parse_mode,omitempty"`
	DisableNotification bool   `json:"disable_notification,omitempty"`
	// TTLSeconds — сколько секунд сообщение актуально; по умолчанию message_ttl.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

type sendMessageResponse struct {
	MessageID int64      `json:"message_id"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req sendMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case (req.Token == "") == (req.Alias == ""):
		writeError(w, http.StatusBadRequest, "exactly one of token or alias is required")
		return
	case strings.TrimSpace(req.Text) == "":
		writeError(w, http.StatusBadRequest, "text is required")
		return
	case utf8.RuneCountInString(req.Text) > maxMessageLen:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("text exceeds %d characters", maxMessageLen))
		return
	case !validParseMode(req.ParseMode):
		writeError(w, http.StatusBadRequest, "parse_mode must be one of: HTML, MarkdownV2, Markdown")
		return
	case req.TTLSeconds < 0:
		writeError(w, http.StatusBadRequest, "ttl_seconds must be positive")
		return
	}

	ctx := r.Context()
	sub, ok := s.findRecipient(w, ctx, req)
	if !ok {
		return
	}
	if sub.Status != storage.StatusActive {
		writeError(w, http.StatusConflict, "subscriber is "+sub.Status)
		return
	}

	msg := storage.NewMessage{
		SubscriberID:        sub.ID,
		Text:                req.Text,
		ParseMode:           req.ParseMode,
		DisableNotification: req.DisableNotification,
	}
	ttl := s.messageTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	resp := sendMessageResponse{Status: storage.MessagePending}
	if ttl > 0 {
		msg.ExpiresAt = time.Now().Add(ttl)
		resp.ExpiresAt = &msg.ExpiresAt
	}

	id, err := s.store.EnqueueMessage(ctx, msg)
	if err != nil {
		s.internalError(w, "enqueue message", err)
		return
	}
	resp.MessageID = id
	w.Header().Set("Location", "/api/v1/messages/"+strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusAccepted, resp)
}

type messageDTO struct {
	MessageID         int64      `json:"message_id"`
	TelegramID        int64      `json:"telegram_id"`
	Status            string     `json:"status"`
	Attempts          int        `json:"attempts"`
	Error             *string    `json:"error,omitempty"`
	TelegramMessageID *int64     `json:"telegram_message_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	NextAttemptAt     *time.Time `json:"next_attempt_at,omitempty"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be an integer")
		return
	}
	m, err := s.store.MessageByID(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		s.internalError(w, "load message", err)
		return
	}
	dto := messageDTO{
		MessageID:         m.ID,
		TelegramID:        m.TelegramID,
		Status:            m.Status,
		Attempts:          m.Attempts,
		Error:             m.Error,
		TelegramMessageID: m.TelegramMessageID,
		CreatedAt:         m.CreatedAt,
		SentAt:            m.SentAt,
		ExpiresAt:         m.ExpiresAt,
	}
	if m.Status == storage.MessagePending {
		dto.NextAttemptAt = &m.NextAttemptAt
	}
	writeJSON(w, http.StatusOK, dto)
}

type createLinkRequest struct {
	Mode       string `json:"mode"` // token | callback
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type createLinkResponse struct {
	Link       string    `json:"link"`
	StartParam string    `json:"start_param"`
	Nonce      string    `json:"nonce"`
	Mode       string    `json:"mode"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// createLink — удобство для службы: то же самое она может сделать сама через pkg/startlink.
func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	var req createLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	mode, err := startlink.ParseMode(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "mode must be token or callback")
		return
	}
	ttl := s.linkTTL
	if req.TTLSeconds != 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl <= 0 || ttl > maxLinkTTL {
		writeError(w, http.StatusBadRequest, "ttl_seconds out of range")
		return
	}

	param, p, err := startlink.New(s.secret, mode, ttl)
	if err != nil {
		s.internalError(w, "create start link", err)
		return
	}
	writeJSON(w, http.StatusOK, createLinkResponse{
		Link:       startlink.DeepLink(s.bot.Username(), param),
		StartParam: param,
		Nonce:      p.NonceHex(),
		Mode:       mode.String(),
		ExpiresAt:  p.ExpiresAt,
	})
}

type subscriberDTO struct {
	TelegramID   int64     `json:"telegram_id"`
	Username     string    `json:"username,omitempty"`
	FirstName    string    `json:"first_name,omitempty"`
	LastName     string    `json:"last_name,omitempty"`
	Status       string    `json:"status"`
	Token        string    `json:"token,omitempty"`
	Nonce        string    `json:"nonce,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
}

func toDTO(s storage.Subscriber) subscriberDTO {
	return subscriberDTO{
		TelegramID:   s.TelegramID,
		Username:     s.Username,
		FirstName:    s.FirstName,
		LastName:     s.LastName,
		Status:       s.Status,
		RegisteredAt: s.CreatedAt,
	}
}

// redeem гасит одноразовый код из callback-URL и отдаёт службе данные подписчика.
func (s *Server) redeem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	sub, nonce, err := s.store.RedeemCallbackCode(r.Context(), storage.HashCallbackCode(req.Code))
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "code is invalid, expired or already used")
		return
	}
	if err != nil {
		s.internalError(w, "redeem callback code", err)
		return
	}
	dto := toDTO(sub)
	dto.Token = sub.Token.String()
	dto.Nonce = hex.EncodeToString(nonce)
	writeJSON(w, http.StatusOK, dto)
}

// resolveToken — для режима token: служба получает telegram_id по токену, введённому пользователем.
func (s *Server) resolveToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := uuid.Parse(strings.TrimSpace(req.Token))
	if err != nil {
		writeError(w, http.StatusBadRequest, "token must be a UUID")
		return
	}
	sub, err := s.store.SubscriberByToken(r.Context(), token)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "subscriber not found")
		return
	}
	if err != nil {
		s.internalError(w, "resolve token", err)
		return
	}
	dto := toDTO(sub)
	dto.Token = sub.Token.String()
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) getSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("telegram_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "telegram_id must be an integer")
		return
	}
	if sub, ok := s.lookup(w, r.Context(), id); ok {
		writeJSON(w, http.StatusOK, toDTO(sub))
	}
}

// --- helpers ---

func (s *Server) lookup(w http.ResponseWriter, ctx context.Context, telegramID int64) (storage.Subscriber, bool) {
	sub, err := s.store.SubscriberByTelegramID(ctx, telegramID)
	return s.loaded(w, sub, err)
}

// findRecipient ищет получателя сообщения по токену или псевдониму.
func (s *Server) findRecipient(w http.ResponseWriter, ctx context.Context, req sendMessageRequest) (storage.Subscriber, bool) {
	if req.Token == "" {
		sub, err := s.store.SubscriberByAlias(ctx, req.Alias)
		return s.loaded(w, sub, err)
	}
	token, err := uuid.Parse(strings.TrimSpace(req.Token))
	if err != nil {
		writeError(w, http.StatusBadRequest, "token must be a UUID")
		return storage.Subscriber{}, false
	}
	sub, err := s.store.SubscriberByToken(ctx, token)
	return s.loaded(w, sub, err)
}

// loaded отвечает ошибкой, если подписчика не удалось загрузить.
func (s *Server) loaded(w http.ResponseWriter, sub storage.Subscriber, err error) (storage.Subscriber, bool) {
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "subscriber not found")
		return sub, false
	}
	if err != nil {
		s.internalError(w, "load subscriber", err)
		return sub, false
	}
	return sub, true
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(key))
		if !ok || subtle.ConstantTimeCompare(got[:], s.apiKeyHash[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

func (s *Server) internalError(w http.ResponseWriter, op string, err error) {
	s.log.Error(op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func validParseMode(m string) bool {
	switch m {
	case "", "HTML", "MarkdownV2", "Markdown":
		return true
	}
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
