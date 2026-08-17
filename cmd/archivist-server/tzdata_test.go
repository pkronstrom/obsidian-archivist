package main

import (
	"os"
	"strings"
	"testing"
)

// The image is FROM scratch: no /usr/share/zoneinfo. Go therefore cannot
// resolve TZ=Europe/Helsinki unless the tzdata is embedded in the binary, and
// the failure is SILENT -- the process falls back to UTC and every commit in the
// vault history is stamped +0000 while the compose file says Helsinki. That
// shipped once and was found only by noticing timestamps three hours behind.
//
// This is a source assertion rather than a behavioural one on purpose. Calling
// time.LoadLocation here would pass on any developer machine and in CI, because
// both have a system zoneinfo; it would only fail in the scratch container,
// where no test runs. The import is the thing that regresses, so the import is
// what this guards.
func TestTzdataIsImported(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `_ "time/tzdata"`) {
		t.Error(`main.go must import _ "time/tzdata": the scratch image has no ` +
			`zoneinfo, so without it TZ is silently ignored and commit ` +
			`timestamps claim a timezone the process is not using`)
	}
}
