package merge

import (
	"encoding/json"
	"reflect"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	return m
}

func TestJSONDisjointKeysBothSurvive(t *testing.T) {
	base := []byte(`{"theme":"obsidian","fontSize":16}`)
	ours := []byte(`{"theme":"minimal","fontSize":16}`)
	theirs := []byte(`{"theme":"obsidian","fontSize":18}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatal("disjoint key edits must not conflict")
	}
	got := decode(t, out)
	if got["theme"] != "minimal" {
		t.Errorf("theme = %v, want minimal", got["theme"])
	}
	if got["fontSize"] != float64(18) {
		t.Errorf("fontSize = %v, want 18", got["fontSize"])
	}
}

func TestJSONSameKeyDifferentValuesConflictsAndTheirsWins(t *testing.T) {
	base := []byte(`{"theme":"obsidian"}`)
	ours := []byte(`{"theme":"minimal"}`)
	theirs := []byte(`{"theme":"things"}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !conflict {
		t.Fatal("a same-key divergence must be reported as a conflict")
	}
	if got := decode(t, out)["theme"]; got != "things" {
		t.Errorf("theme = %v, want things (the last writer)", got)
	}
}

func TestJSONIdenticalEditsAreNotAConflict(t *testing.T) {
	base := []byte(`{"theme":"obsidian"}`)
	ours := []byte(`{"theme":"minimal"}`)
	theirs := []byte(`{"theme":"minimal"}`)

	_, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatal("both sides making the same edit is not a conflict")
	}
}

// Arrays replace wholesale, NEVER merge. Merging a hotkey list or an enabled
// plugin list element by element produces a combination neither device asked
// for, and nothing reports it.
func TestJSONArraysReplaceWholesale(t *testing.T) {
	base := []byte(`{"enabled":["a","b"]}`)
	ours := []byte(`{"enabled":["a","b","c"]}`)
	theirs := []byte(`{"enabled":["a","b","d"]}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !conflict {
		t.Fatal("two different array edits must conflict, not union")
	}
	got := decode(t, out)["enabled"]
	want := []any{"a", "b", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("enabled = %v, want %v (theirs, whole)", got, want)
	}
}

func TestJSONOneSidedArrayEditApplies(t *testing.T) {
	base := []byte(`{"enabled":["a"]}`)
	ours := []byte(`{"enabled":["a"]}`)
	theirs := []byte(`{"enabled":["a","b"]}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatal("only one side changed the array")
	}
	if !reflect.DeepEqual(decode(t, out)["enabled"], []any{"a", "b"}) {
		t.Errorf("the one-sided array edit was lost")
	}
}

func TestJSONNestedObjectsMergeByKey(t *testing.T) {
	base := []byte(`{"a":{"x":1,"y":2}}`)
	ours := []byte(`{"a":{"x":9,"y":2}}`)
	theirs := []byte(`{"a":{"x":1,"y":8}}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatal("disjoint nested edits must not conflict")
	}
	a := decode(t, out)["a"].(map[string]any)
	if a["x"] != float64(9) || a["y"] != float64(8) {
		t.Errorf("nested merge = %v, want x=9 y=8", a)
	}
}

func TestJSONAdditionsFromBothSidesSurvive(t *testing.T) {
	base := []byte(`{}`)
	ours := []byte(`{"a":1}`)
	theirs := []byte(`{"b":2}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatal("two additions of different keys must not conflict")
	}
	got := decode(t, out)
	if got["a"] != float64(1) || got["b"] != float64(2) {
		t.Errorf("merged = %v, want both additions", got)
	}
}

// A deletion on one side against an edit on the other keeps the edit. Same bias
// as the reconciler's own: content is recoverable, an absence is not.
func TestJSONDeleteVersusEditKeepsTheEdit(t *testing.T) {
	base := []byte(`{"k":"old"}`)
	ours := []byte(`{}`)
	theirs := []byte(`{"k":"new"}`)

	out, _, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(t, out)["k"]; got != "new" {
		t.Errorf("k = %v, want new", got)
	}
}

func TestJSONOneSidedDeleteApplies(t *testing.T) {
	base := []byte(`{"k":"old","j":1}`)
	ours := []byte(`{"k":"old","j":1}`)
	theirs := []byte(`{"j":1}`)

	out, conflict, err := MergeJSON(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatal("a one-sided delete is not a conflict")
	}
	if _, present := decode(t, out)["k"]; present {
		t.Error("the deletion was not applied")
	}
}

// Anything that is not a JSON object falls back to whole-file, which the caller
// then treats as last-writer-wins with a conflict copy.
func TestJSONNonObjectFallsBack(t *testing.T) {
	for _, tc := range [][3][]byte{
		{[]byte(`{}`), []byte(`not json`), []byte(`{}`)},
		{[]byte(`[]`), []byte(`[]`), []byte(`[1]`)},
		{[]byte(``), []byte(`{}`), []byte(`{"a":1}`)},
		// `null` parses into a map with NO error and leaves it nil, so it must
		// be rejected explicitly or the merge writes {} over it and calls that
		// a clean result.
		{[]byte(`{}`), []byte(`null`), []byte(`{"a":1}`)},
		{[]byte(`{}`), []byte(`{"a":1}`), []byte(`null`)},
	} {
		out, conflict, err := MergeJSON(tc[0], tc[1], tc[2])
		if err != nil {
			t.Fatalf("fallback must not error: %v", err)
		}
		if !conflict {
			t.Errorf("unparseable input must be reported as a conflict: %q", tc[1])
		}
		if string(out) != string(tc[2]) {
			t.Errorf("fallback result = %q, want theirs %q", out, tc[2])
		}
	}
}

// The output must be JSON Obsidian will read back: two-space indent and a
// trailing newline, matching what it writes itself.
func TestJSONOutputShape(t *testing.T) {
	out, _, err := MergeJSON([]byte(`{"a":1}`), []byte(`{"a":2}`), []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 2\n}\n"
	if string(out) != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}
