package main

import (
	"os"
	"strings"
	"testing"
)

// alpine ships no tzdata, so TZ is silently ignored without the embedded copy
// and the relay logs UTC while the server it fronts logs local time. Same
// source-level guard as the server's, and for the same reason: time.LoadLocation
// would pass here and in CI, and only fail inside the container where no test
// runs.
func TestTzdataIsImported(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `_ "time/tzdata"`) {
		t.Error(`main.go must import _ "time/tzdata": alpine has no zoneinfo, so ` +
			`without it TZ is ignored and the relay's logs disagree with the server's`)
	}
}
