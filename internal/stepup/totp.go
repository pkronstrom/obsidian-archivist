// Package stepup is proof of presence: a one-time code, what it authorises, and
// for how long.
//
// It knows nothing about HTTP, vaults or git. Everything here takes a secret, a
// code and a clock and returns a verdict, which is what makes it testable
// without a server and reusable for a second kind of authorisation later.
// See docs/adr/0004-step-up-has-two-lifetimes.md.
package stepup

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Step is the TOTP time step. 30 seconds is what every authenticator assumes,
// which is why it is not configurable.
const Step = 30 * time.Second

// SkewSteps is how many steps either side of now are accepted.
//
// One. Zero fails whenever you start typing at second 28; two is five times the
// guess surface for drift that NTP already removes.
const SkewSteps = 1

const digits = 6

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a fresh base32 secret, 160 bits as RFC 4226 recommends.
func NewSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("stepup: generating a secret: %w", err)
	}
	return encoding.EncodeToString(raw), nil
}

// Code renders the code for one instant.
func Code(secret string, at time.Time) (string, error) {
	key, err := decode(secret)
	if err != nil {
		return "", err
	}
	return code(key, at.Unix()/int64(Step/time.Second)), nil
}

// Verify reports whether presented is valid at `at`, and which step matched.
//
// The caller MUST remember the returned step and refuse a repeat. A code is only
// worthless after use if something enforces that; see Verifier.
func Verify(secret, presented string, at time.Time) (int64, bool) {
	key, err := decode(secret)
	if err != nil {
		return 0, false
	}
	presented = strings.TrimSpace(presented)
	now := at.Unix() / int64(Step/time.Second)
	for offset := int64(-SkewSteps); offset <= SkewSteps; offset++ {
		step := now + offset
		// Constant time: this compares against a value an attacker is actively
		// guessing, unlike the token lookup which compares digests.
		if subtle.ConstantTimeCompare([]byte(code(key, step)), []byte(presented)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// URI renders the otpauth:// URL an authenticator consumes.
//
// The label is escaped because it is operator-supplied and lands in two nesting
// contexts at once: a URL, and the single-quoted shell command the mint output
// prints around it. A quote would end that command and start another; an
// ampersand would make the rest of the label look like a query parameter.
func URI(label, secret string) string {
	return fmt.Sprintf("otpauth://totp/Archivist:%s?secret=%s&issuer=Archivist",
		escapeLabel(label), url.QueryEscape(secret))
}

// escapeLabel percent-encodes everything outside RFC 3986's unreserved set.
//
// url.PathEscape is not enough: it leaves sub-delimiters such as & and ' alone
// because they are legal in a path segment. Legal is not the bar here -- the
// label is operator-supplied text being spliced into a URL that is itself
// spliced into a shell command, so anything that could be read as structure in
// either context has to go.
func escapeLabel(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func decode(secret string) ([]byte, error) {
	key, err := encoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return nil, fmt.Errorf("stepup: secret is not base32: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("stepup: secret is empty")
	}
	return key, nil
}

func code(key []byte, step int64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}
