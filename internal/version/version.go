// Package version identifies the build and the wire protocol.
//
// These are two different numbers on purpose. Version moves whenever anything
// ships; Protocol moves only when the wire format changes in a way a client
// must care about. A client checks Protocol and ignores Version, so ordinary
// releases never look like breaking changes to it.
package version

// Version is the build. Set at link time:
//
//	go build -ldflags "-X github.com/pkronstrom/obsidian-archivist/internal/version.Version=1.2.3"
var Version = "dev"

// Protocol is the wire contract. Increment ONLY on a change a client cannot
// tolerate -- a removed field, a changed meaning, a new required parameter.
// Adding an optional field does not count: clients ignore what they do not know.
//
//	1  initial: head, snapshot, changes, have, content, push, events, history,
//	   at, check, export. Changes and events share one enriched shape.
const Protocol = 1
