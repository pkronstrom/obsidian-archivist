package reconcile

import (
	"bytes"
	"unicode/utf8"

	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// AppendNote appends suffix to the current text at path in one locked mutation.
// An empty expectedHash intentionally means "append to whatever is current".
func (rc *Reconciler) AppendNote(path string, suffix []byte, expectedHash string, origin Origin) (head, contentHash string, err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.normalizeNFC {
		path = vault.ToNFC(path)
	}
	if err := validateMutationPath(path); err != nil {
		return "", "", err
	}
	if len(suffix) == 0 {
		return "", "", mutationError(protocol.CodeMalformed, "append content is empty")
	}

	body, err := currentText(rc.v, path, expectedHash)
	if err != nil {
		return "", "", err
	}
	if len(body) > protocol.MaxUploadBytes || len(suffix) > protocol.MaxUploadBytes-len(body) {
		return "", "", mutationError(protocol.CodeTooLarge, "resulting note is too large")
	}

	next := make([]byte, len(body)+len(suffix))
	copy(next, body)
	copy(next[len(body):], suffix)
	if err := rc.admit(path, int64(len(next))); err != nil {
		return "", "", err
	}
	previousHead, err := rc.r.Head()
	if err != nil {
		return "", "", err
	}
	if err := rc.write(path, next); err != nil {
		return "", "", err
	}
	head, err = rc.r.Commit(origin.message("append from"))
	if err != nil {
		return "", "", err
	}
	rc.notify(previousHead, head)
	return head, protocol.HashContent(next), nil
}

// EditNote replaces the one exact occurrence of oldText in the current text at
// path. expectedHash is required so the replacement cannot land on unseen text.
func (rc *Reconciler) EditNote(path, expectedHash string, oldText, newText []byte, origin Origin) (head, contentHash string, err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.normalizeNFC {
		path = vault.ToNFC(path)
	}
	if err := validateMutationPath(path); err != nil {
		return "", "", err
	}
	if expectedHash == "" {
		return "", "", mutationError(protocol.CodeMalformed, "content revision is required")
	}
	if len(oldText) == 0 {
		return "", "", mutationError(protocol.CodeMalformed, "old text is empty")
	}
	if bytes.Equal(oldText, newText) {
		return "", "", mutationError(protocol.CodeMalformed, "old text and new text are identical")
	}

	body, err := currentText(rc.v, path, expectedHash)
	if err != nil {
		return "", "", err
	}
	switch matches := overlappingMatchCount(body, oldText); matches {
	case 0:
		return "", "", mutationError(protocol.CodeNoMatch, "old text was not found")
	case 1:
	default:
		return "", "", mutationError(protocol.CodeMultipleMatches, "old text occurs more than once")
	}

	remaining := len(body) - len(oldText)
	if remaining > protocol.MaxUploadBytes || len(newText) > protocol.MaxUploadBytes-remaining {
		return "", "", mutationError(protocol.CodeTooLarge, "resulting note is too large")
	}
	next := bytes.Replace(body, oldText, newText, 1)
	if err := rc.admit(path, int64(len(next))); err != nil {
		return "", "", err
	}
	previousHead, err := rc.r.Head()
	if err != nil {
		return "", "", err
	}
	if err := rc.write(path, next); err != nil {
		return "", "", err
	}
	head, err = rc.r.Commit(origin.message("edit from"))
	if err != nil {
		return "", "", err
	}
	rc.notify(previousHead, head)
	return head, protocol.HashContent(next), nil
}

func validateMutationPath(path string) error {
	if err := vault.ValidPath(path); err != nil {
		return mutationError(protocol.CodeInvalidPath, err.Error())
	}
	if vault.Skip(path) {
		return mutationError(protocol.CodeForbidden, path+" is excluded from sync")
	}
	return nil
}

func currentText(v *vault.Vault, path, expectedHash string) ([]byte, error) {
	info, err := v.Stat(path)
	if err != nil {
		return nil, mutationError(protocol.CodeNotFound, "no such note: "+path)
	}
	if info.Size() > int64(protocol.MaxUploadBytes) {
		return nil, mutationError(protocol.CodeTooLarge, "note is too large")
	}
	body, err := v.Read(path)
	if err != nil {
		return nil, mutationError(protocol.CodeNotFound, "no such note: "+path)
	}
	if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
		return nil, mutationError(protocol.CodeNotText, path+" is not UTF-8 text")
	}
	if expectedHash != "" && protocol.HashContent(body) != expectedHash {
		return nil, mutationError(protocol.CodeStale, path+" changed since it was read")
	}
	return body, nil
}

// overlappingMatchCount returns 0, 1, or 2, where 2 means multiple.
// Advancing one byte after a match includes overlapping occurrences.
func overlappingMatchCount(body, oldText []byte) int {
	matches := 0
	for offset := 0; offset+len(oldText) <= len(body); {
		index := bytes.Index(body[offset:], oldText)
		if index < 0 {
			return matches
		}
		matches++
		if matches == 2 {
			return matches
		}
		offset += index + 1
	}
	return matches
}

func mutationError(code, message string) error {
	return &protocol.Error{Code: code, Message: message}
}
