package protocol

import "testing"

// Codes are stable wire identifiers. A duplicate would make two distinct
// failures indistinguishable to a client.
func TestErrorCodesAreDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []string{
		CodeUnauthorized, CodeUnknownBase, CodeInvalidPath, CodeMissingContent,
		CodeHashMismatch, CodeTooLarge, CodeMalformed, CodeNotFound,
		CodeDuplicatePath, CodeInternal, CodeQuarantined, CodeThrottled,
		CodeDiskLow,
	} {
		if c == "" {
			t.Fatal("an error code is empty")
		}
		if seen[c] {
			t.Fatalf("duplicate error code %q", c)
		}
		seen[c] = true
	}
}
