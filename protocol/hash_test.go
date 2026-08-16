package protocol

import (
	"os/exec"
	"strings"
	"testing"
)

// The oracle is git itself, so no expected value here is invented.
func TestHashContentMatchesGitHashObject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, in := range []string{
		"hello\n",
		"",
		"ä 🎉\n", // non-ASCII: catches a character-count header
		"# note\n\nbody\n",
		"no trailing newline",
		strings.Repeat("x", 100000),
	} {
		out, err := runGit(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := HashContent([]byte(in)); got != out {
			t.Errorf("HashContent(%.20q) = %s, git says %s", in, got, out)
		}
	}
}

func runGit(in string) (string, error) {
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
