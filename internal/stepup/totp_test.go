package stepup_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
)

// RFC 6238 appendix B uses the ASCII secret "12345678901234567890", which is
// this in base32.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestCodeMatchesRFC6238(t *testing.T) {
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
	}
	for _, tc := range cases {
		got, err := stepup.Code(rfcSecret, time.Unix(tc.unix, 0))
		if err != nil {
			t.Fatalf("Code(%d): %v", tc.unix, err)
		}
		if got != tc.want {
			t.Errorf("Code(%d) = %q, want %q", tc.unix, got, tc.want)
		}
	}
}

func TestVerifyAcceptsOneStepOfSkew(t *testing.T) {
	now := time.Unix(1111111109, 0)
	for _, offset := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		code, err := stepup.Code(rfcSecret, now.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stepup.Verify(rfcSecret, code, now); !ok {
			t.Errorf("code from offset %s was refused", offset)
		}
	}
	for _, offset := range []time.Duration{-60 * time.Second, 60 * time.Second} {
		code, err := stepup.Code(rfcSecret, now.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stepup.Verify(rfcSecret, code, now); ok {
			t.Errorf("code from offset %s was accepted; skew is one step", offset)
		}
	}
}

func TestVerifyReportsTheMatchedStep(t *testing.T) {
	now := time.Unix(1234567890, 0)
	code, err := stepup.Code(rfcSecret, now)
	if err != nil {
		t.Fatal(err)
	}
	step, ok := stepup.Verify(rfcSecret, code, now)
	if !ok {
		t.Fatal("a freshly generated code was refused")
	}
	if want := int64(1234567890 / 30); step != want {
		t.Errorf("step = %d, want %d", step, want)
	}
}

func TestNewSecretIsUsableBase32(t *testing.T) {
	s, err := stepup.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 32 {
		t.Errorf("secret length = %d, want 32", len(s))
	}
	if _, err := stepup.Code(s, time.Unix(1, 0)); err != nil {
		t.Fatalf("a generated secret did not round-trip: %v", err)
	}
}

func TestGarbageSecretIsAnError(t *testing.T) {
	if _, err := stepup.Code("not!base32", time.Unix(1, 0)); err == nil {
		t.Error("a non-base32 secret produced a code")
	}
	if _, ok := stepup.Verify("not!base32", "000000", time.Unix(1, 0)); ok {
		t.Error("a non-base32 secret verified a code")
	}
}

// The label is operator-controlled and lands in a URL that the mint output wraps
// in a single-quoted shell command. A quote or an ampersand must not escape
// either context.
func TestURIEscapesTheLabel(t *testing.T) {
	got := stepup.URI("agent's box & co", rfcSecret)
	if strings.Contains(got, "'") {
		t.Errorf("URI leaves a single quote in the label, which breaks out of the shell command: %s", got)
	}
	if strings.Count(got, "&") != 1 {
		t.Errorf("URI leaves a bare & in the label, which becomes a query parameter: %s", got)
	}
}
