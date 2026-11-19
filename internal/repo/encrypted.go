package repo

import (
	"errors"
	"fmt"
)

// The vault kind lives in the git config, as archivist.encrypted.
//
// In the git directory rather than beside it because Archive already ships
// `config`: a vault restored from an export comes back encrypted, with its
// guards, and nothing has to remember to copy a marker. And never inside the
// work tree, where a client holding only the write scope could change it.
const (
	configSection   = "archivist"
	configEncrypted = "encrypted"
)

// ErrHasHistory is SetEncrypted refusing a vault that already has commits.
var ErrHasHistory = errors.New("repo: the vault already has history")

// Encrypted reports whether this vault accepts only enveloped content.
func (r *Repo) Encrypted() (bool, error) {
	cfg, err := r.git.Config()
	if err != nil {
		return false, fmt.Errorf("repo: reading config: %w", err)
	}
	return cfg.Raw.Section(configSection).Option(configEncrypted) == "true", nil
}

// SetEncrypted marks the vault as encrypted. Only before the first commit.
//
// A vault with history already holds plaintext objects, and marking it would
// claim a property its own past contradicts. There is deliberately no way to
// clear the mark either: that would let one config edit reopen the vault to
// plaintext writes.
func (r *Repo) SetEncrypted() error {
	head, err := r.Head()
	if err != nil {
		return err
	}
	if head != "" {
		return ErrHasHistory
	}
	cfg, err := r.git.Config()
	if err != nil {
		return fmt.Errorf("repo: reading config: %w", err)
	}
	cfg.Raw.SetOption(configSection, "", configEncrypted, "true")
	if err := r.git.SetConfig(cfg); err != nil {
		return fmt.Errorf("repo: writing config: %w", err)
	}
	return nil
}
