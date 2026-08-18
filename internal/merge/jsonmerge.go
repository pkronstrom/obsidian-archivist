package merge

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// MergeJSON is a three-way merge that works on keys rather than lines.
//
// A settings file merged as TEXT can come out syntactically valid and
// semantically wrong -- two devices editing adjacent keys produce a file diff3
// is happy with and Obsidian reads as a configuration nobody chose. Nothing
// reports it, and the oddness surfaces days later. Keys are the unit these
// files are actually edited in, so they are the unit to merge in.
//
// conflict is true when both sides changed the same key to different values.
// The caller still gets a usable result -- theirs wins, because they are the
// last writer -- and is expected to keep a conflict copy of ours alongside it.
//
// Anything that is not a JSON object on all three sides falls back to theirs,
// reported as a conflict. That is the whole-file behaviour, kept deliberately:
// it is what this function degrades to, and what remains if key merging is ever
// judged not to have earned its place.
func MergeJSON(base, ours, theirs []byte) (result []byte, conflict bool, err error) {
	var b, o, t map[string]any
	if json.Unmarshal(base, &b) != nil ||
		json.Unmarshal(ours, &o) != nil ||
		json.Unmarshal(theirs, &t) != nil {
		return theirs, true, nil
	}
	// A literal `null` unmarshals into a map WITHOUT error and leaves it nil.
	// Without this check the merge would treat null as an empty object and
	// happily write `{}` over it, reporting no conflict -- silently replacing
	// valid non-object JSON, which is exactly what the whole-file fallback
	// exists to prevent. `ours` and `theirs` must both be real objects; a nil
	// base is fine and means "no common ancestor".
	if o == nil || t == nil {
		return theirs, true, nil
	}

	merged, conflicted := mergeObject(b, o, t)
	out, err := encode(merged)
	if err != nil {
		return theirs, true, nil
	}
	return out, conflicted, nil
}

// mergeObject walks the union of both sides' keys against the base.
func mergeObject(base, ours, theirs map[string]any) (map[string]any, bool) {
	out := map[string]any{}
	conflict := false

	keys := map[string]struct{}{}
	for k := range ours {
		keys[k] = struct{}{}
	}
	for k := range theirs {
		keys[k] = struct{}{}
	}

	for k := range keys {
		bv, inBase := base[k]
		ov, inOurs := ours[k]
		tv, inTheirs := theirs[k]

		ourChanged := !inOurs || !inBase || !equal(bv, ov)
		theirChanged := !inTheirs || !inBase || !equal(bv, tv)

		switch {
		case !theirChanged:
			// Only we touched it (or neither did).
			if inOurs {
				out[k] = ov
			}
		case !ourChanged:
			if inTheirs {
				out[k] = tv
			}
		case equal(ov, tv):
			// Identical edits on both sides are not a conflict.
			if inOurs {
				out[k] = ov
			}
		case !inOurs && inTheirs:
			// We deleted, they edited. Keep the edit: content is recoverable,
			// an absence is not. Same bias as the reconciler's own resolve.
			out[k] = tv
		case inOurs && !inTheirs:
			out[k] = ov
		default:
			// Both changed it to something different.
			om, ourIsObj := ov.(map[string]any)
			tm, theirIsObj := tv.(map[string]any)
			bm, _ := bv.(map[string]any)
			if ourIsObj && theirIsObj {
				sub, subConflict := mergeObject(bm, om, tm)
				out[k] = sub
				conflict = conflict || subConflict
				continue
			}
			// Arrays land here and are REPLACED wholesale, never merged.
			// Merging a hotkey list or an enabled-plugin list element by element
			// produces a combination neither device asked for.
			out[k] = tv
			conflict = true
		}
	}
	return out, conflict
}

func equal(a, b any) bool { return reflect.DeepEqual(a, b) }

// encode writes the shape Obsidian writes: two-space indent, a trailing
// newline, and no HTML escaping (a snippet selector containing "&" must survive
// a round trip).
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
