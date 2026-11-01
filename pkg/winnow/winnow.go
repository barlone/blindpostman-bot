// Package winnow — порт функций Chaff и Winnow (aWinnow.pas, FreePascal, little-endian)
// и обёрток TangleText и UnTangleText (Autils.pas).
//
// Это обфускация для совместимости с существующими конфигурациями, а не криптография:
// ключа нет, и значение расшифровывается любым, у кого есть этот код.
//
// Несколько особенностей исходного кода сохранены намеренно, иначе значения не совпадут:
//   - маска в хеше $F000000, а биты очищаются уже сдвинутым значением;
//   - RORDWord(H, 1) не меняет H, поэтому смещение символа постоянно для всей строки;
//   - символы хранятся в обратном порядке;
//   - байты вне диапазона 32..127 шифром пропускаются и счётчик cx не уменьшают.
package winnow

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"strings"
)

// ErrInvalid — значение повреждено или зашифровано не этим алгоритмом.
var ErrInvalid = errors.New("winnow: invalid value")

const (
	b64Table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

	chaffPerChar = 8 // на каждый символ исходной строки приходится 8 байт результата
	hashSize     = 6 // длина закодированного хеша в конце результата
	tangleMask   = 0x3A
	// minTangled — минимальная длина значения, которое UnTangleText считает зашифрованным.
	minTangled = 28
)

var encipherTable = [96]byte{
	75, 85, 86, 92, 93, 95, 74, 76, 84, 87, 91, 94,
	63, 73, 77, 83, 88, 90, 62, 64, 72, 78, 82, 89,
	51, 61, 65, 71, 79, 81, 50, 52, 60, 66, 70, 80,
	39, 49, 53, 59, 67, 69, 38, 40, 48, 54, 58, 68,
	27, 37, 41, 47, 55, 57, 26, 28, 36, 42, 46, 56,
	15, 25, 29, 35, 43, 45, 14, 16, 24, 30, 34, 44,
	6, 13, 17, 23, 31, 33, 5, 7, 12, 18, 22, 32,
	1, 4, 8, 11, 19, 21, 0, 2, 3, 9, 10, 20,
}

var decipherTable = [96]byte{
	90, 84, 91, 92, 85, 78, 72, 79, 86, 93, 94, 87,
	80, 73, 66, 60, 67, 74, 81, 88, 95, 89, 82, 75,
	68, 61, 54, 48, 55, 62, 69, 76, 83, 77, 70, 63,
	56, 49, 42, 36, 43, 50, 57, 64, 71, 65, 58, 51,
	44, 37, 30, 24, 31, 38, 45, 52, 59, 53, 46, 39,
	32, 25, 18, 12, 19, 26, 33, 40, 47, 41, 34, 27,
	20, 13, 6, 0, 7, 14, 21, 28, 35, 29, 22, 15,
	8, 1, 2, 9, 16, 23, 17, 10, 3, 4, 11, 5,
}

// Chaff шифрует src и подмешивает случайные байты. Пустой src даёт пустой результат.
func Chaff(src []byte) []byte {
	n := len(src)
	if n == 0 {
		return nil
	}
	buf := append([]byte(nil), src...)
	encipher(buf)
	h := hash(buf)

	out := make([]byte, n*chaffPerChar, n*chaffPerChar+hashSize)
	for i := range out {
		k := rand.IntN(224)
		if k < 32 {
			k |= 32
		}
		out[i] = byte(k)
	}
	k := offset(h)
	for i, j := n-1, 0; i >= 0; i, j = i-1, j+chaffPerChar {
		out[j+k] = buf[i]
	}
	return append(out, encodeInt(h)...)
}

// Winnow — обратная операция к Chaff. В отличие от Паскаля, где результатом ошибки
// становится пустая строка, здесь она возвращается явно.
func Winnow(src []byte) ([]byte, error) {
	body := len(src) - hashSize
	if body < chaffPerChar {
		return nil, ErrInvalid
	}
	h := decodeInt(src[body:])
	k := offset(h)

	n := body / chaffPerChar
	out := make([]byte, n)
	for i, j := n-1, 0; i >= 0; i, j = i-1, j+chaffPerChar {
		out[i] = src[j+k]
	}
	if hash(out) != h {
		return nil, ErrInvalid
	}
	decipher(out)
	return out, nil
}

// Tangle готовит значение для конфигурации: {HEX(XOR $3A(Chaff(text)))}.
// Фигурные скобки добавляет сам: TangleText их не ставит, но UnTangleText требует.
func Tangle(text string) string {
	data := Chaff([]byte(text))
	xorBytes(data)
	return "{" + strings.ToUpper(hex.EncodeToString(data)) + "}"
}

// Untangle — порт UnTangleText. Значение не в фигурных скобках возвращается как есть:
// так в конфигурации можно указать и незашифрованный пароль.
func Untangle(text string) (string, error) {
	if len(text) < minTangled || text[0] != '{' || text[len(text)-1] != '}' {
		return text, nil
	}
	data, err := hex.DecodeString(text[1 : len(text)-1])
	if err != nil {
		return "", fmt.Errorf("%w: bad hex", ErrInvalid)
	}
	xorBytes(data)
	out, err := Winnow(data)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func xorBytes(b []byte) {
	for i := range b {
		b[i] ^= tangleMask
	}
}

// offset — позиция значащего байта внутри каждого блока.
func offset(h uint32) int {
	return int(bits.RotateLeft32(h, -1) & 7)
}

func hash(src []byte) uint32 {
	var r uint32
	for _, b := range src {
		r <<= 4
		r += uint32(b)
		bx := r & 0x0F000000
		if bx != 0 {
			bx >>= 24
			r ^= bx
		}
		r &= ^bx
	}
	return r
}

// encodeInt — EnCodeInt: NtoBE на little-endian меняет порядок байтов.
func encodeInt(x uint32) []byte {
	x = bits.ReverseBytes32(x)
	out := make([]byte, hashSize)
	for i := range out {
		out[i] = b64Table[x&0x3F]
		x >>= 6
	}
	return out
}

func decodeInt(src []byte) uint32 {
	var r uint32
	for i, c := range src {
		if p := strings.IndexByte(b64Table, c); p >= 0 {
			r |= uint32(p)
		}
		n := 6
		if i == hashSize-1 {
			n = 2
		}
		r = bits.RotateLeft32(r, -n)
	}
	return bits.ReverseBytes32(r)
}

func encipher(s []byte) {
	dx, cx := uint32(len(s)), uint32(len(s))
	for i, c := range s {
		if c < 32 || c > 127 {
			continue
		}
		b := encipherTable[c-32]
		if cx&3 != 0 {
			dx = bits.RotateLeft32(dx, 3)
		}
		dx &= 0xFFFFFF1F
		b ^= byte(dx)
		dx += cx + uint32(b)
		s[i] = b + 32
		cx--
	}
}

func decipher(s []byte) {
	dx, cx := uint32(len(s)), uint32(len(s))
	for i, c := range s {
		if c < 32 || c > 127 {
			continue
		}
		b := c - 32
		if cx&3 != 0 {
			dx = bits.RotateLeft32(dx, 3)
		}
		dx &= 0xFFFFFF1F
		plain := b ^ byte(dx)
		dx += cx + uint32(b)
		s[i] = decipherTable[plain] + 32
		cx--
	}
}
