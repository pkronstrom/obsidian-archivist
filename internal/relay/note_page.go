package relay

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

const (
	noteCursorVersion = byte(1)
	noteCursorBytes   = 17
	defaultNoteChars  = 16_000
	maxNoteChars      = 100_000
)

var (
	errInvalidCursor = errors.New("invalid note cursor")
	errStaleCursor   = errors.New("stale note cursor")
)

type noteCursor struct {
	hashPrefix [12]byte
	offset     uint32
}

type notePage struct {
	content    string
	nextOffset uint32
	hasMore    bool
}

type validatedNote struct {
	body []byte
}

func encodeNoteCursor(contentHash string, offset uint32) (string, error) {
	prefix, err := noteHashPrefix(contentHash)
	if err != nil {
		return "", err
	}

	var raw [noteCursorBytes]byte
	raw[0] = noteCursorVersion
	copy(raw[1:13], prefix[:])
	binary.BigEndian.PutUint32(raw[13:17], offset)
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func validNoteCursorEncodedLength(n int) bool {
	return n == base64.RawURLEncoding.EncodedLen(noteCursorBytes)
}

func decodeNoteCursor(token string) (noteCursor, error) {
	if !validNoteCursorEncodedLength(len(token)) {
		return noteCursor{}, fmt.Errorf("%w: encoded length is %d, want %d",
			errInvalidCursor, len(token), base64.RawURLEncoding.EncodedLen(noteCursorBytes))
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return noteCursor{}, fmt.Errorf("%w: malformed base64url", errInvalidCursor)
	}
	if len(raw) != noteCursorBytes {
		return noteCursor{}, fmt.Errorf("%w: decoded length is %d, want %d", errInvalidCursor, len(raw), noteCursorBytes)
	}
	if raw[0] != noteCursorVersion {
		return noteCursor{}, fmt.Errorf("%w: unsupported version %d", errInvalidCursor, raw[0])
	}

	var cursor noteCursor
	copy(cursor.hashPrefix[:], raw[1:13])
	cursor.offset = binary.BigEndian.Uint32(raw[13:17])
	return cursor, nil
}

func validateNoteCursor(cursor noteCursor, contentHash string) error {
	prefix, err := noteHashPrefix(contentHash)
	if err != nil {
		return err
	}
	if cursor.hashPrefix != prefix {
		return errStaleCursor
	}
	return nil
}

func noteHashPrefix(contentHash string) ([12]byte, error) {
	var prefix [12]byte
	if len(contentHash) != 40 {
		return prefix, fmt.Errorf("%w: Git blob hash must contain 40 hexadecimal characters", errInvalidCursor)
	}
	raw, err := hex.DecodeString(contentHash)
	if err != nil {
		return prefix, fmt.Errorf("%w: invalid Git blob hash", errInvalidCursor)
	}
	copy(prefix[:], raw[:12])
	return prefix, nil
}

func checkedNoteOffset(bodyLen, offset uint64) (uint32, error) {
	if bodyLen > math.MaxUint32 {
		return 0, fmt.Errorf("%w: body length %d exceeds uint32", errInvalidCursor, bodyLen)
	}
	if offset > math.MaxUint32 {
		return 0, fmt.Errorf("%w: byte offset %d exceeds uint32", errInvalidCursor, offset)
	}
	if offset > bodyLen {
		return 0, fmt.Errorf("%w: byte offset %d exceeds body length %d", errInvalidCursor, offset, bodyLen)
	}
	return uint32(offset), nil
}

func validateNoteText(body []byte) (validatedNote, error) {
	for offset := 0; offset < len(body); {
		if body[offset] == 0 {
			return validatedNote{}, fmt.Errorf("note body contains NUL at byte offset %d", offset)
		}
		r, size := utf8.DecodeRune(body[offset:])
		if r == utf8.RuneError && size == 1 {
			return validatedNote{}, fmt.Errorf("note body is not valid UTF-8 at byte offset %d", offset)
		}
		offset += size
	}
	return validatedNote{body: body}, nil
}

func pageNote(note validatedNote, start uint32, maxChars int) (notePage, error) {
	body := note.body
	if maxChars <= 0 {
		return notePage{}, fmt.Errorf("max_chars must be positive")
	}
	start, err := checkedNoteOffset(uint64(len(body)), uint64(start))
	if err != nil {
		return notePage{}, err
	}

	if start < uint32(len(body)) && !utf8.RuneStart(body[start]) {
		return notePage{}, fmt.Errorf("%w: byte offset %d is not on a UTF-8 boundary", errInvalidCursor, start)
	}

	end := int(start)
	for chars := 0; chars < maxChars && end < len(body); chars++ {
		r, size := utf8.DecodeRune(body[end:])
		if r == 0 {
			return notePage{}, fmt.Errorf("note body contains NUL at byte offset %d", end)
		}
		if r == utf8.RuneError && size == 1 {
			return notePage{}, fmt.Errorf("note body is not valid UTF-8 at byte offset %d", end)
		}
		end += size
	}

	return notePage{
		content:    string(body[int(start):end]),
		nextOffset: uint32(end),
		hasMore:    end < len(body),
	}, nil
}

func offsetForLine(body []byte, line int) (uint32, error) {
	if line < 1 {
		return 0, fmt.Errorf("start_line must be one-based")
	}
	if _, err := checkedNoteOffset(uint64(len(body)), 0); err != nil {
		return 0, err
	}
	if !utf8.Valid(body) {
		return 0, fmt.Errorf("note body is not valid UTF-8")
	}
	if line == 1 {
		return 0, nil
	}

	currentLine := 1
	for offset, b := range body {
		if b != '\n' {
			continue
		}
		currentLine++
		if currentLine == line {
			return uint32(offset + 1), nil
		}
	}
	return 0, fmt.Errorf("start_line %d is beyond the last line", line)
}
