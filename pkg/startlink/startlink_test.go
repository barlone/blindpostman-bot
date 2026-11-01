package startlink

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

// Telegram допускает в параметре /start только эти символы, до 64 штук.
var startParamRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func TestRoundTrip(t *testing.T) {
	for _, mode := range []Mode{ModeToken, ModeCallback} {
		s, p, err := New(secret, mode, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !startParamRe.MatchString(s) {
			t.Fatalf("param %q is not a valid Telegram start parameter", s)
		}
		got, err := Decode(secret, s, time.Now())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got != p {
			t.Fatalf("got %+v, want %+v", got, p)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	valid, _, err := New(secret, ModeToken, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired, _, err := New(secret, ModeToken, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(valid)
	tampered[3] ^= 1

	tests := []struct {
		name   string
		secret []byte
		param  string
		want   error
	}{
		{"wrong secret", []byte("another-secret-another-secret-xx"), valid, ErrBadSignature},
		{"tampered", secret, string(tampered), ErrBadSignature},
		{"garbage", secret, "hello", ErrMalformed},
		{"not base64", secret, "!!!!", ErrMalformed},
		{"expired", secret, expired, ErrExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.secret, tt.param, time.Now()); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}
