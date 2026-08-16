package repo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Two archives of an unchanged repository must be byte-identical, or every
// backup snapshot stores a fresh copy instead of deduplicating against the
// last one.
func TestUncompressedArchiveIsReproducible(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("one\n"))
	if _, err := r.Commit("first"); err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	if err := r.Archive(&a, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Archive(&b, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("archives differ: %d vs %d bytes", a.Len(), b.Len())
	}
}

func TestArchiveRestoresToAWorkingRepository(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("one\n"))
	r.Commit("first")
	v.Write("b.md", []byte("two\n"))
	want, err := r.Commit("second")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := r.Archive(&buf, true); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty archive")
	}

	// Unpack and check with real git -- the only test that matters here is
	// whether a restored copy is usable.
	dest := t.TempDir()
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dest, hdr.Name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	out, err := exec.Command("git", "--git-dir="+dest, "fsck", "--no-progress").CombinedOutput()
	if err != nil {
		t.Fatalf("fsck on the restored archive failed: %v: %s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "error") || strings.HasPrefix(line, "missing") {
			t.Errorf("fsck reported: %s", line)
		}
	}
	head, err := exec.Command("git", "--git-dir="+dest, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(string(head)); got != want {
		t.Errorf("restored HEAD = %s, want %s", got, want)
	}
	blob, err := exec.Command("git", "--git-dir="+dest, "show", "HEAD:b.md").Output()
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if string(blob) != "two\n" {
		t.Errorf("restored content = %q", blob)
	}
}
