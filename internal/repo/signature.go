package repo

import (
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
)

// signature is fixed and explicit. A scratch container has no git config, no
// /etc/passwd and no HOME, so anything inferred from the environment would fail
// there while working fine in tests.
func signature() *object.Signature {
	return &object.Signature{
		Name:  "archivist",
		Email: "archivist@localhost",
		When:  time.Now(),
	}
}
