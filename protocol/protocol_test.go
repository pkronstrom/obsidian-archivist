package protocol

import (
	"encoding/json"
	"testing"
)

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

func TestAppendNoteRequestJSON(t *testing.T) {
	want := AppendNoteRequest{
		Path:            "Triage.md",
		Content:         "\nentry",
		ContentRevision: "abc",
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `{"path":"Triage.md","content":"\nentry","content_revision":"abc"}` {
		t.Fatalf("append request = %s", got)
	}

	var got AppendNoteRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("unmarshaled append request = %#v, want %#v", got, want)
	}

	body, err = json.Marshal(AppendNoteRequest{Path: "Triage.md", Content: "\nentry"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `{"path":"Triage.md","content":"\nentry"}` {
		t.Fatalf("append request without content revision = %s", got)
	}
}

func TestEditNoteRequestJSON(t *testing.T) {
	want := EditNoteRequest{
		Path:            "Triage.md",
		ContentRevision: "abc",
		OldText:         "old",
		NewText:         "new",
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `{"path":"Triage.md","content_revision":"abc","old_text":"old","new_text":"new"}` {
		t.Fatalf("edit request = %s", got)
	}

	var got EditNoteRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("unmarshaled edit request = %#v, want %#v", got, want)
	}
}

func TestNoteMutationResponseJSON(t *testing.T) {
	want := NoteMutationResponse{
		Path:            "Triage.md",
		Status:          StatusApplied,
		Revision:        "commit",
		ContentRevision: "blob",
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `{"path":"Triage.md","status":"applied","revision":"commit","content_revision":"blob"}` {
		t.Fatalf("note mutation response = %s", got)
	}

	var got NoteMutationResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("unmarshaled note mutation response = %#v, want %#v", got, want)
	}
}
