package protocol

import "testing"

func TestStepUpRequiredIsItsOwnCode(t *testing.T) {
	if CodeStepUpRequired == CodeForbidden {
		t.Fatal("step-up collapses into forbidden; a client cannot tell " +
			"'ask for a code' from 'you will never be allowed'")
	}
	if CodeStepUpRequired == "" {
		t.Fatal("the code is empty")
	}
}

// Step-up is additive: an unmarked token's wire contract is byte-identical
// before and after, so the version must not move.
func TestProtocolVersionIsUnchanged(t *testing.T) {
	if Version != 2 {
		t.Errorf("Version = %d, want 2", Version)
	}
}
