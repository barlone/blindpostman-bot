package winnow

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

// Эталоны получены вызовом Chaff из настоящего awinnow.pas, собранного FPC под x86_64.
var goldens = []struct{ plain, chaffed string }{
	{"password", "ACAA509F388CA77F7E3F623D269E8EA7483B84DE3F2348BA3E8A20362E994C59AAAAD9A450A12879" +
		"C0224579407B27A5D44969274D32A93FA091A36424288D98524F31485943"},
	{"P@ss w0rd!~", "32C33A2FC0BB34A96D23A12167D68DD9383230C7B131318C2BAA509FDE8CA77F703F623DA99E8EA7" +
		"3B3B84DE5C2348BA7B8A20363E994C5949AAD9A4D2A1287950224579247B27A54C4969274A32A93F" +
		"2391A36445288D98504977334141"},
	{"Пароль", "8C862572B791DDD9D1C33A2FC0BB34A9BB23A12167D68DD9D03230C7B131318CBEAA509FDE8CA77F" +
		"D03F623DA99E8EA7803B84DE5C2348BAD18A20363E994C59B0AAD9A4D2A12879D0224579247B27A5" +
		"9F4969274A32A93FD091A36445288D98642B37764143"},
}

func TestWinnowGolden(t *testing.T) {
	for _, g := range goldens {
		data, err := hex.DecodeString(g.chaffed)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Winnow(data)
		if err != nil {
			t.Fatalf("%q: %v", g.plain, err)
		}
		if string(got) != g.plain {
			t.Errorf("got %q, want %q", got, g.plain)
		}
	}
}

func TestChaffRoundTrip(t *testing.T) {
	for range 2000 {
		src := make([]byte, 1+rand.IntN(64))
		for i := range src {
			src[i] = byte(rand.IntN(256))
		}
		data := Chaff(src)
		if len(data) != len(src)*chaffPerChar+hashSize {
			t.Fatalf("len %d for source of %d", len(data), len(src))
		}
		got, err := Winnow(data)
		if err != nil || !bytes.Equal(got, src) {
			t.Fatalf("src %q: got %q, err %v", src, got, err)
		}
	}
}

func TestTablesAreInverse(t *testing.T) {
	for i, v := range encipherTable {
		if int(decipherTable[v]) != i {
			t.Fatalf("decipherTable[encipherTable[%d]] = %d", i, decipherTable[v])
		}
	}
}

func TestWinnowRejects(t *testing.T) {
	corrupted := Chaff([]byte("password"))
	corrupted[len(corrupted)-1] = 'A' + (corrupted[len(corrupted)-1]-'A'+1)%26
	for name, src := range map[string][]byte{
		"empty":     nil,
		"too short": []byte("abcdefghijklm"),
		"corrupted": corrupted,
	} {
		if _, err := Winnow(src); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	if Chaff(nil) != nil {
		t.Error("Chaff of empty input must be empty")
	}
}

func TestUntangle(t *testing.T) {
	tangled := Tangle("s3cret")
	if tangled[0] != '{' || tangled != strings.ToUpper(tangled) {
		t.Fatalf("unexpected form %q", tangled)
	}
	if got, err := Untangle(tangled); err != nil || got != "s3cret" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if got, err := Untangle(strings.ToLower(tangled)); err != nil || got != "s3cret" {
		t.Fatalf("lower-case hex: got %q, err %v", got, err)
	}

	// Как в UnTangleText: всё, что не похоже на {HEX}, — незашифрованное значение.
	for _, plain := range []string{"", "postman", "{short}", "no-braces-but-long-enough-to-be-tangled"} {
		if got, err := Untangle(plain); err != nil || got != plain {
			t.Errorf("%q: got %q, err %v", plain, got, err)
		}
	}

	for name, bad := range map[string]string{
		"not hex":   "{" + strings.Repeat("Z", 30) + "}",
		"bad value": "{" + strings.Repeat("A", 40) + "}",
	} {
		if _, err := Untangle(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}
