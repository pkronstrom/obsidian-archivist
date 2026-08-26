package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func mustValidateNoteText(t *testing.T, body []byte) validatedNote {
	t.Helper()
	note, err := validateNoteText(body)
	if err != nil {
		t.Fatalf("validateNoteText: %v", err)
	}
	return note
}

func TestPageCursorIsShortURLSafeAndDeterministic(t *testing.T) {
	hash := "000102030405060708090a0b0c0d0e0f10111213"
	const offset = uint32(0x89abcdef)

	first, err := encodeNoteCursor(hash, offset)
	if err != nil {
		t.Fatalf("encodeNoteCursor: %v", err)
	}
	second, err := encodeNoteCursor(hash, offset)
	if err != nil {
		t.Fatalf("encodeNoteCursor again: %v", err)
	}
	if first != second {
		t.Fatalf("same cursor input encoded differently: %q then %q", first, second)
	}
	if len(first) != 23 {
		t.Fatalf("cursor length = %d, want 23: %q", len(first), first)
	}
	if strings.ContainsAny(first, "+/=") {
		t.Fatalf("cursor is not unpadded base64url: %q", first)
	}
	if _, err := base64.RawURLEncoding.DecodeString(first); err != nil {
		t.Fatalf("cursor is not raw URL-safe base64: %v", err)
	}
}

func TestPageCursorContainsFullPrefixAndUint32Offset(t *testing.T) {
	hash := "000102030405060708090a0b0c0d0e0f10111213"
	const offset = uint32(math.MaxUint32)

	token, err := encodeNoteCursor(hash, offset)
	if err != nil {
		t.Fatalf("encodeNoteCursor: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode raw cursor: %v", err)
	}
	if len(raw) != 17 {
		t.Fatalf("decoded cursor length = %d, want 17", len(raw))
	}
	if raw[0] != 1 {
		t.Errorf("cursor version = %d, want 1", raw[0])
	}
	wantHash, err := hex.DecodeString(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[1:13], wantHash[:12]) {
		t.Errorf("hash prefix = %x, want all 12 bytes %x", raw[1:13], wantHash[:12])
	}
	if got := binary.BigEndian.Uint32(raw[13:17]); got != offset {
		t.Errorf("offset = %#x, want %#x", got, offset)
	}

	cursor, err := decodeNoteCursor(token)
	if err != nil {
		t.Fatalf("decodeNoteCursor: %v", err)
	}
	if !bytes.Equal(cursor.hashPrefix[:], wantHash[:12]) {
		t.Errorf("decoded hash prefix = %x, want %x", cursor.hashPrefix, wantHash[:12])
	}
	if cursor.offset != offset {
		t.Errorf("decoded offset = %d, want %d", cursor.offset, offset)
	}
}

func TestPageCursorRejectsMalformedTokensWithTypedError(t *testing.T) {
	wrongVersion := make([]byte, 17)
	wrongVersion[0] = 2

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "malformed base64", token: "not!base64"},
		{name: "empty decoded value", token: ""},
		{name: "decoded value too short", token: base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
		{name: "decoded value too long", token: base64.RawURLEncoding.EncodeToString(make([]byte, 18))},
		{name: "unsupported version", token: base64.RawURLEncoding.EncodeToString(wrongVersion)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeNoteCursor(tc.token); !errors.Is(err, errInvalidCursor) {
				t.Fatalf("decodeNoteCursor error = %v, want errInvalidCursor", err)
			}
		})
	}
}

func TestPageCursorEncodedLengthIsExactly23(t *testing.T) {
	for _, length := range []int{0, 22, 24, math.MaxInt} {
		if validNoteCursorEncodedLength(length) {
			t.Errorf("length %d was accepted, want only 23", length)
		}
	}
	if !validNoteCursorEncodedLength(23) {
		t.Error("length 23 was rejected")
	}
}

func TestPageCursorRejectsInvalidGitHashes(t *testing.T) {
	for _, hash := range []string{
		"",
		strings.Repeat("0", 39),
		strings.Repeat("0", 41),
		strings.Repeat("g", 40),
	} {
		if _, err := encodeNoteCursor(hash, 0); !errors.Is(err, errInvalidCursor) {
			t.Errorf("encodeNoteCursor(%q) error = %v, want errInvalidCursor", hash, err)
		}
	}
}

func TestPageCursorValidationUsesTheFull96BitPrefix(t *testing.T) {
	original := "000102030405060708090a0b0c0d0e0f10111213"
	samePrefix := "000102030405060708090a0bffffffffffffffff"
	differentTwelfthByte := "000102030405060708090aff0c0d0e0f10111213"

	token, err := encodeNoteCursor(original, 0)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeNoteCursor(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNoteCursor(cursor, samePrefix); err != nil {
		t.Fatalf("hash with the same 96-bit prefix was rejected: %v", err)
	}
	if err := validateNoteCursor(cursor, differentTwelfthByte); !errors.Is(err, errStaleCursor) {
		t.Fatalf("hash differing in the twelfth prefix byte returned %v, want errStaleCursor", err)
	}
}

func TestPageCursorDetectsChangedContentAsStale(t *testing.T) {
	original := []byte("alpha\nβeta\n")
	token, err := encodeNoteCursor(protocol.HashContent(original), 6)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeNoteCursor(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNoteCursor(cursor, protocol.HashContent(original)); err != nil {
		t.Fatalf("unchanged content was rejected: %v", err)
	}
	changed := []byte("alpha\nβeta changed\n")
	if err := validateNoteCursor(cursor, protocol.HashContent(changed)); !errors.Is(err, errStaleCursor) {
		t.Fatalf("changed content returned %v, want errStaleCursor", err)
	}
}

func TestPageCursorOffsetBeyondBodyIsInvalid(t *testing.T) {
	body := []byte("short")
	note := mustValidateNoteText(t, body)
	token, err := encodeNoteCursor(protocol.HashContent(body), uint32(len(body)+1))
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeNoteCursor(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pageNote(note, cursor.offset, 10); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("pageNote error = %v, want errInvalidCursor", err)
	}
}

func TestNotePageCountsUnicodeCodePointsAndEndsOnBoundary(t *testing.T) {
	body := []byte("aβ🙂界z")
	note := mustValidateNoteText(t, body)
	page, err := pageNote(note, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if page.content != "aβ🙂界" {
		t.Errorf("content = %q, want %q", page.content, "aβ🙂界")
	}
	if got := utf8.RuneCountInString(page.content); got != 4 {
		t.Errorf("page has %d Unicode code points, want 4", got)
	}
	if !utf8.ValidString(page.content) {
		t.Errorf("page is not valid UTF-8: %q", page.content)
	}
	if page.nextOffset != uint32(len([]byte(page.content))) {
		t.Errorf("next offset = %d, want %d", page.nextOffset, len([]byte(page.content)))
	}
	if !page.hasMore {
		t.Error("hasMore is false with one code point remaining")
	}
}

func TestNotePagesConcatenateExactlyAcrossChangingSizes(t *testing.T) {
	body := []byte("Aβ🙂\nsecond 界 line\n끝")
	note := mustValidateNoteText(t, body)
	sizes := []int{1, 4, 2, 5, 3}
	var joined []byte
	var start uint32

	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > utf8.RuneCount(body)+1 {
			t.Fatal("pagination did not terminate")
		}
		maxChars := sizes[pageNumber%len(sizes)]
		page, err := pageNote(note, start, maxChars)
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber, err)
		}
		if !utf8.ValidString(page.content) {
			t.Fatalf("page %d is not valid UTF-8: %q", pageNumber, page.content)
		}
		if got := utf8.RuneCountInString(page.content); got > maxChars {
			t.Fatalf("page %d has %d code points, limit %d", pageNumber, got, maxChars)
		}
		if page.nextOffset < start || page.nextOffset > uint32(len(body)) {
			t.Fatalf("page %d moved offset from %d to %d for %d-byte body", pageNumber, start, page.nextOffset, len(body))
		}
		if got, want := []byte(page.content), body[start:page.nextOffset]; !bytes.Equal(got, want) {
			t.Fatalf("page %d content = %q, exact source range = %q", pageNumber, got, want)
		}
		if page.nextOffset < uint32(len(body)) && !utf8.RuneStart(body[page.nextOffset]) {
			t.Fatalf("page %d next offset %d is in the middle of a code point", pageNumber, page.nextOffset)
		}
		joined = append(joined, page.content...)
		if !page.hasMore {
			if page.nextOffset != uint32(len(body)) {
				t.Fatalf("last page stopped at %d, want %d", page.nextOffset, len(body))
			}
			break
		}
		if page.nextOffset == start {
			t.Fatalf("page %d reports more content without advancing", pageNumber)
		}
		start = page.nextOffset
	}

	if !bytes.Equal(joined, body) {
		t.Fatalf("joined pages = %q, want exact body %q", joined, body)
	}
}

func TestNotePageAtEmptyBodyOrEndIsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  []byte
		start uint32
	}{
		{name: "empty body", body: nil, start: 0},
		{name: "end of body", body: []byte("aβ"), start: uint32(len([]byte("aβ")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := mustValidateNoteText(t, tc.body)
			page, err := pageNote(note, tc.start, 10)
			if err != nil {
				t.Fatal(err)
			}
			if page.content != "" || page.hasMore || page.nextOffset != tc.start {
				t.Errorf("end page = %+v, want empty content, no more, offset %d", page, tc.start)
			}
		})
	}
}

func TestNotePageRejectsOutOfRangeAndMidRuneOffsets(t *testing.T) {
	body := []byte("aβc")
	note := mustValidateNoteText(t, body)
	for _, tc := range []struct {
		name   string
		offset uint32
	}{
		{name: "past end", offset: uint32(len(body) + 1)},
		{name: "middle of beta", offset: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pageNote(note, tc.offset, 1); !errors.Is(err, errInvalidCursor) {
				t.Fatalf("pageNote error = %v, want errInvalidCursor", err)
			}
		})
	}
}

func TestValidateNoteTextRejectsInvalidUTF8Anywhere(t *testing.T) {
	body := append([]byte(strings.Repeat("valid ", 100)), 0xff)
	if _, err := validateNoteText(body); err == nil {
		t.Fatal("invalid UTF-8 beyond the first page was accepted as text")
	}
}

func TestValidateNoteTextRejectsNULAnywhere(t *testing.T) {
	body := append([]byte(strings.Repeat("valid ", 100)), 0)
	if _, err := validateNoteText(body); err == nil {
		t.Fatal("NUL beyond the first page was accepted as text")
	}
}

func TestNotePageWalksOnlyBoundedPartOfValidatedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  byte
	}{
		{name: "invalid UTF-8", bad: 0xff},
		{name: "NUL", bad: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("αβx")
			note := mustValidateNoteText(t, body)
			body[len([]byte("αβ"))] = tc.bad

			first, err := pageNote(note, 0, 2)
			if err != nil {
				t.Fatalf("bounded first page inspected later bytes: %v", err)
			}
			if first.content != "αβ" || !first.hasMore {
				t.Fatalf("first page = %+v, want valid content and hasMore", first)
			}
			if first.nextOffset != uint32(len([]byte("αβ"))) {
				t.Fatalf("first next offset = %d, want %d", first.nextOffset, len([]byte("αβ")))
			}
			if _, err := pageNote(note, first.nextOffset, 1); err == nil {
				t.Fatalf("paging into post-validation %s byte succeeded", tc.name)
			}
		})
	}
}

func TestNotePageOffsetGuardRejectsValuesBeyondUint32WithoutAllocating(t *testing.T) {
	tooLarge := uint64(math.MaxUint32) + 1
	for _, tc := range []struct {
		name    string
		bodyLen uint64
		offset  uint64
	}{
		{name: "body length", bodyLen: tooLarge, offset: 0},
		{name: "offset width", bodyLen: math.MaxUint32, offset: tooLarge},
		{name: "offset past body", bodyLen: 9, offset: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := checkedNoteOffset(tc.bodyLen, tc.offset); !errors.Is(err, errInvalidCursor) {
				t.Fatalf("checkedNoteOffset(%d, %d) error = %v, want errInvalidCursor", tc.bodyLen, tc.offset, err)
			}
		})
	}
	if got, err := checkedNoteOffset(math.MaxUint32, math.MaxUint32); err != nil || got != math.MaxUint32 {
		t.Fatalf("maximum uint32 offset = %d, %v; want %d, nil", got, err, uint64(math.MaxUint32))
	}
}

func TestNotePageStartLineIsOneBased(t *testing.T) {
	body := []byte("first\nβeta\nthird\nlast")
	note := mustValidateNoteText(t, body)
	for _, tc := range []struct {
		line int
		want uint32
	}{
		{line: 1, want: 0},
		{line: 2, want: uint32(len([]byte("first\n")))},
		{line: 3, want: uint32(len([]byte("first\nβeta\n")))},
		{line: 4, want: uint32(len([]byte("first\nβeta\nthird\n")))},
	} {
		got, err := offsetForLine(note, tc.line)
		if err != nil {
			t.Fatalf("offsetForLine(line %d): %v", tc.line, err)
		}
		if got != tc.want {
			t.Errorf("offsetForLine(line %d) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func TestNotePageLineAfterTrailingNewlineIsValid(t *testing.T) {
	body := []byte("first\n")
	note := mustValidateNoteText(t, body)
	got, err := offsetForLine(note, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != uint32(len(body)) {
		t.Errorf("line 2 offset = %d, want EOF offset %d", got, len(body))
	}
	page, err := pageNote(note, got, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.content != "" || page.hasMore {
		t.Errorf("trailing empty line page = %+v, want empty final page", page)
	}
}

func TestNotePageRejectsNonPositiveOrBeyondLastLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		line int
	}{
		{name: "zero", body: []byte("one\ntwo"), line: 0},
		{name: "negative", body: []byte("one\ntwo"), line: -1},
		{name: "after final nonempty line", body: []byte("one\ntwo"), line: 3},
		{name: "after trailing empty line", body: []byte("one\n"), line: 3},
		{name: "after sole empty line", body: nil, line: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := mustValidateNoteText(t, tc.body)
			if _, err := offsetForLine(note, tc.line); err == nil {
				t.Fatalf("offsetForLine(%q, %d) succeeded", tc.body, tc.line)
			}
		})
	}
}

func TestReadNotePageWholeBodyTextRefusalKeepsContextualGuidance(t *testing.T) {
	validPrefix := []byte(strings.Repeat("a", 8_001))
	invalidBodies := []struct {
		name string
		tail byte
	}{
		{name: "NUL after binary heuristic window", tail: 0},
		{name: "invalid UTF-8 after binary heuristic window", tail: 0xff},
	}
	contexts := []struct {
		tool      string
		want      []string
		forbidden []string
	}{
		{
			tool: "read_note",
			want: []string{"read_note only returns text", "read_attachment"},
		},
		{
			tool:      "read_note_at",
			want:      []string{"read_note_at", "historical attachment retrieval is unavailable"},
			forbidden: []string{"read_attachment"},
		},
	}

	for _, contextCase := range contexts {
		for _, bodyCase := range invalidBodies {
			t.Run(contextCase.tool+"/"+bodyCase.name, func(t *testing.T) {
				body := append(append([]byte(nil), validPrefix...), bodyCase.tail)
				_, err := pageReadNote(body, "repository-revision", contextCase.tool, readInput{Path: "notes/raw.md"})
				if err == nil {
					t.Fatal("pageReadNote returned a page from an invalid text snapshot")
				}
				got := err.Error()
				for _, want := range contextCase.want {
					if !strings.Contains(got, want) {
						t.Errorf("refusal = %q, want %q", got, want)
					}
				}
				for _, forbidden := range contextCase.forbidden {
					if strings.Contains(got, forbidden) {
						t.Errorf("refusal = %q, must not suggest %q", got, forbidden)
					}
				}
			})
		}
	}
}

func TestNonTextReadErrorGuidanceMatchesToolContextWithoutCause(t *testing.T) {
	for _, tc := range []struct {
		tool      string
		want      []string
		forbidden []string
	}{
		{
			tool: "read_note",
			want: []string{"read_note only returns text", "read_attachment"},
		},
		{
			tool:      "read_note_at",
			want:      []string{"read_note_at", "historical attachment retrieval is unavailable"},
			forbidden: []string{"read_attachment"},
		},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			got := nonTextReadError(tc.tool, "notes/raw.md", 9_001, nil).Error()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("refusal = %q, want %q", got, want)
				}
			}
			for _, forbidden := range tc.forbidden {
				if strings.Contains(got, forbidden) {
					t.Errorf("refusal = %q, must not suggest %q", got, forbidden)
				}
			}
			if strings.Contains(got, "<nil>") {
				t.Errorf("refusal exposes an absent cause: %q", got)
			}
		})
	}
}
