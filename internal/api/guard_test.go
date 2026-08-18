package api

import (
	"net/http"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func TestStatusForGuardCodes(t *testing.T) {
	for _, tc := range []struct {
		code string
		want int
	}{
		{protocol.CodeQuarantined, http.StatusTooManyRequests},
		{protocol.CodeThrottled, http.StatusTooManyRequests},
		{protocol.CodeDiskLow, http.StatusInsufficientStorage},
		{"something else", http.StatusInternalServerError},
	} {
		if got := statusFor(tc.code); got != tc.want {
			t.Errorf("statusFor(%q) = %d, want %d", tc.code, got, tc.want)
		}
	}
}
