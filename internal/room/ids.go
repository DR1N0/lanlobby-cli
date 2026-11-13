package room

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NewID returns a random 16-byte session identifier, hex encoded.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Body validates a chat message: 1–8 KiB of UTF-8 text.
func validateBody(value string) error {
	if value == "" || len(value) > 8192 || !utf8.ValidString(value) {
		return fmt.Errorf("message must be 1–8192 UTF-8 bytes")
	}
	return nil
}

// SafeText keeps output plain: newlines, tabs, control and formatting
// characters are escaped so one message stays on one line.
func SafeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == '\u2028' || r == '\u2029':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
