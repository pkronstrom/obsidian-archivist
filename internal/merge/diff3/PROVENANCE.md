# Vendored: github.com/epiclabs-io/diff3

Copied verbatim from `github.com/epiclabs-io/diff3`
at `v0.0.0-20260520111523-3b1669897fb1`, MIT licensed (see LICENSE).

Only `diff3.go` and `myersdiff.go` are needed.

## Why vendored rather than imported

Two files and 21 KB, sitting directly in the data-integrity path, from a
16-star repository. It is cheaper to own and audit than to depend on.

## Validation

Compared against `git merge-file` on twelve cases before adoption:
disjoint edits, same-line conflict, identical change on both sides, insert at
the same position, delete-vs-edit, adjacent appends, one-sided change, missing
trailing newline, empty base, unicode, a 500-line file, and whitespace-only
changes.

**Result: 12 agree, 0 disagree** on the conflict verdict.

## Known defect, worked around in the parent package

**It drops the trailing newline.** `git merge-file` preserves it. Left
unhandled, every merged note would lose its final newline and show up as a
spurious diff on the next sync.

The fix lives in `../merge.go`, never in these files, so re-vendoring upstream
stays a clean copy.

## Re-vendoring

    go mod download github.com/epiclabs-io/diff3
    cp $(go env GOMODCACHE)/github.com/epiclabs-io/diff3@<ver>/{diff3,myersdiff}.go .

Then run the parent package's tests; they encode the twelve cases above.
