package protocol

import "testing"

func TestClientOlderThan(t *testing.T) {
	for _, tc := range []struct {
		header, min string
		want        bool
	}{
		{"plugin/0.20.0", "0.21.0", true},
		{"plugin/0.9.9", "0.10.0", true}, // numeric, not lexical
		{"plugin/0.21.0", "0.21.0", false},
		{"plugin/0.21.1", "0.21.0", false},
		{"plugin/1.0.0", "0.21.0", false},
		{"plugin/v0.20.0", "v0.21.0", true},
		// Nothing below can be read as "older", so none of it is refused.
		{"plugin/0.20.0", "", false},
		{"", "0.21.0", false},
		{"plugin", "0.21.0", false},
		{"plugin/dev", "0.21.0", false},
		{"plugin/0.20", "0.21.0", false},
		{"relay/0.1.0", "0.21.0", false},
	} {
		if got := ClientOlderThan(tc.header, ClientPlugin, tc.min); got != tc.want {
			t.Errorf("ClientOlderThan(%q, plugin, %q) = %v, want %v", tc.header, tc.min, got, tc.want)
		}
	}
}
