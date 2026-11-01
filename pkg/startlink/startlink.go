// Package startlink генерирует и проверяет параметр команды /start
// (deep link https://t.me/<bot>?start=<param>), подписанный общим секретом
// бота и регистрирующей службы.
//
// Пакет лежит в pkg/, чтобы регистрирующая служба могла генерировать ссылки
// сама, не обращаясь к API бота.
//
// Формат параметра (до кодирования base64url без паддинга):
//
//	[0]      версия формата (1)
//	[1]      режим регистрации (Mode)
//	[2:6]    срок действия, unix-секунды, big-endian uint32
//	[6:14]   случайный nonce (делает ссылку одноразовой)
//	[14:30]  HMAC-SHA256(secret, bytes[0:14]), усечённый до 16 байт
//
// Итого 30 байт → 40 символов: укладывается в лимит Telegram
// (до 64 символов из набора A-Z, a-z, 0-9, _ и -).
package startlink

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"time"
)

const (
	formatVersion = 1
	NonceSize     = 8
	bodySize      = 2 + 4 + NonceSize
	sigSize       = 16
	rawSize       = bodySize + sigSize

	// MinSecretSize — минимальная длина общего секрета в байтах.
	MinSecretSize = 32
)

// Mode определяет, что бот выдаст пользователю после регистрации.
type Mode byte

const (
	// ModeToken — бот показывает пользователю UUID-токен авторизации.
	ModeToken Mode = 1
	// ModeCallback — бот показывает одноразовый URL обратного вызова службы.
	ModeCallback Mode = 2
)

func (m Mode) String() string {
	switch m {
	case ModeToken:
		return "token"
	case ModeCallback:
		return "callback"
	default:
		return fmt.Sprintf("unknown(%d)", byte(m))
	}
}

func (m Mode) valid() bool { return m == ModeToken || m == ModeCallback }

// ParseMode разбирает строковое имя режима ("token" или "callback").
func ParseMode(s string) (Mode, error) {
	switch s {
	case "token":
		return ModeToken, nil
	case "callback":
		return ModeCallback, nil
	default:
		return 0, fmt.Errorf("startlink: unknown mode %q", s)
	}
}

// Payload — содержимое параметра /start.
type Payload struct {
	Mode      Mode
	ExpiresAt time.Time
	Nonce     [NonceSize]byte
}

// NonceHex возвращает nonce в hex: по нему служба может сопоставить
// выданную ссылку со своим пользователем.
func (p Payload) NonceHex() string { return hex.EncodeToString(p.Nonce[:]) }

var (
	ErrMalformed    = errors.New("startlink: malformed payload")
	ErrBadSignature = errors.New("startlink: bad signature")
	ErrExpired      = errors.New("startlink: payload expired")
)

// New создаёт параметр со случайным nonce, действующий ttl.
func New(secret []byte, mode Mode, ttl time.Duration) (string, Payload, error) {
	p := Payload{Mode: mode, ExpiresAt: time.Now().Add(ttl).Truncate(time.Second)}
	if _, err := rand.Read(p.Nonce[:]); err != nil {
		return "", Payload{}, fmt.Errorf("startlink: generate nonce: %w", err)
	}
	s, err := Encode(secret, p)
	if err != nil {
		return "", Payload{}, err
	}
	return s, p, nil
}

// Encode подписывает и кодирует payload.
func Encode(secret []byte, p Payload) (string, error) {
	if !p.Mode.valid() {
		return "", fmt.Errorf("startlink: unknown mode %d", byte(p.Mode))
	}
	exp := p.ExpiresAt.Unix()
	if exp <= 0 || exp > math.MaxUint32 {
		return "", fmt.Errorf("startlink: expiry out of range: %v", p.ExpiresAt)
	}

	buf := make([]byte, rawSize)
	buf[0] = formatVersion
	buf[1] = byte(p.Mode)
	binary.BigEndian.PutUint32(buf[2:6], uint32(exp))
	copy(buf[6:bodySize], p.Nonce[:])
	copy(buf[bodySize:], sign(secret, buf[:bodySize]))
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Decode проверяет подпись и срок действия параметра.
// При ErrExpired возвращается разобранный payload.
func Decode(secret []byte, s string, now time.Time) (Payload, error) {
	buf, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(buf) != rawSize || buf[0] != formatVersion {
		return Payload{}, ErrMalformed
	}
	if !hmac.Equal(buf[bodySize:], sign(secret, buf[:bodySize])) {
		return Payload{}, ErrBadSignature
	}

	p := Payload{
		Mode:      Mode(buf[1]),
		ExpiresAt: time.Unix(int64(binary.BigEndian.Uint32(buf[2:6])), 0),
	}
	copy(p.Nonce[:], buf[6:bodySize])
	if !p.Mode.valid() {
		return Payload{}, ErrMalformed
	}
	if !now.Before(p.ExpiresAt) {
		return p, ErrExpired
	}
	return p, nil
}

// DeepLink собирает ссылку на бота с параметром /start.
func DeepLink(botUsername, param string) string {
	return "https://t.me/" + url.PathEscape(botUsername) + "?start=" + param
}

func sign(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return m.Sum(nil)[:sigSize]
}
