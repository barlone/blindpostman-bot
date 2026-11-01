// Package config загружает настройки из INI-файла.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/blind-postman/blind-postman/pkg/startlink"
	"github.com/blind-postman/blind-postman/pkg/winnow"
)

// DefaultPath — файл настроек по умолчанию.
const DefaultPath = "config.ini"

type Config struct {
	TelegramToken string
	// DeveloperID — Telegram ID пользователя, для которого в логике бота действуют исключения.
	// 0 — такого пользователя нет.
	DeveloperID int64
	DatabaseURL string
	// SharedSecret — общий секрет бота и регистрирующей службы для подписи ссылок /start.
	SharedSecret []byte
	// APIKey — ключ доступа службы к REST API (заголовок Authorization: Bearer <key>).
	APIKey   string
	HTTPAddr string
	// CallbackURL — адрес службы, куда ведёт одноразовая ссылка (?code=...).
	// Пустой — режим callback отключён.
	CallbackURL string
	CallbackTTL time.Duration
	// LinkTTL — срок действия ссылок, выдаваемых через API по умолчанию.
	LinkTTL time.Duration
	// MessageTTL — срок актуальности сообщения по умолчанию; после него недоставленное
	// сообщение получает статус expired. 0 — бессрочно.
	MessageTTL time.Duration

	Queue Queue
}

// Queue — настройки доставки из очереди.
type Queue struct {
	Workers      int
	MaxAttempts  int
	GlobalRate   float64       // сообщений в секунду на бота
	ChatInterval time.Duration // между сообщениями в один чат
	PollInterval time.Duration
}

// Load читает настройки из INI-файла path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	ini, err := parseINI(data)
	if err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}

	r := &reader{ini: ini}
	c := Config{
		TelegramToken: r.required("telegram", "bot_token"),
		DeveloperID:   int64(r.integer("telegram", "developer_id", 0)),
		DatabaseURL:   r.databaseURL(),
		SharedSecret:  []byte(r.required("connect", "link_shared_secret")),
		APIKey:        r.required("config", "api_token"),
		HTTPAddr:      r.str("config", "listen", ":8080"),
		CallbackURL:   r.str("connect", "callback_url", ""),
		CallbackTTL:   r.duration("connect", "callback_ttl", 10*time.Minute),
		LinkTTL:       r.duration("connect", "link_ttl", 24*time.Hour),
		Queue: Queue{
			Workers:      r.integer("send-queue", "queue_workers", 8),
			MaxAttempts:  r.integer("send-queue", "queue_max_attempts", 10),
			GlobalRate:   r.float("telegram", "global_rate", 25),
			ChatInterval: r.duration("telegram", "chat_interval", time.Second),
			PollInterval: r.duration("send-queue", "queue_poll_interval", time.Second),
		},
	}
	if r.str("alerts", "message_ttl", "") != "0" {
		c.MessageTTL = r.duration("alerts", "message_ttl", time.Hour)
	}

	if n := len(c.SharedSecret); n > 0 && n < startlink.MinSecretSize {
		r.fail("[connect] link_shared_secret must be at least %d bytes", startlink.MinSecretSize)
	}
	if n := len(c.APIKey); n > 0 && n < 16 {
		r.fail("[config] api_token must be at least 16 characters")
	}
	if c.CallbackURL != "" {
		if u, err := url.Parse(c.CallbackURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			r.fail("[connect] callback_url must be an absolute http(s) URL")
		}
	}
	return c, errors.Join(r.errs...)
}

// reader достаёт значения, накапливая ошибки, чтобы показать все проблемы файла сразу.
type reader struct {
	ini  iniFile
	errs []error
}

func (r *reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reader) str(section, key, def string) string {
	if v, ok := r.ini[section][key]; ok && v != "" {
		return v
	}
	return def
}

func (r *reader) required(section, key string) string {
	v := r.str(section, key, "")
	if v == "" {
		r.fail("[%s] %s is required", section, key)
	}
	return v
}

func (r *reader) duration(section, key string, def time.Duration) time.Duration {
	v := r.str(section, key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		r.fail("[%s] %s: invalid duration %q", section, key, v)
	}
	return d
}

func (r *reader) integer(section, key string, def int) int {
	v := r.str(section, key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		r.fail("[%s] %s: must be a positive integer, got %q", section, key, v)
	}
	return n
}

func (r *reader) float(section, key string, def float64) float64 {
	v := r.str(section, key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		r.fail("[%s] %s: must be a positive number, got %q", section, key, v)
	}
	return f
}

// databaseURL собирает строку подключения из секции [database].
// Пароль либо зашифрован (см. пакет winnow), либо указан как есть.
func (r *reader) databaseURL() string {
	const section = "database"
	host := r.required(section, "host")
	name := r.required(section, "name")
	user := r.required(section, "user")
	port := r.integer(section, "port", 5432)
	if port > 65535 {
		r.fail("[%s] port out of range", section)
	}

	password, err := winnow.Untangle(r.required(section, "password"))
	if err != nil {
		// Значение пароля в сообщение не попадает.
		r.fail("[%s] password: cannot decrypt: %w", section, err)
	}

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + name,
	}
	if mode := r.str(section, "sslmode", ""); mode != "" {
		u.RawQuery = url.Values{"sslmode": {mode}}.Encode()
	}
	return u.String()
}
