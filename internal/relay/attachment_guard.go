package relay

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// What an agent may upload, and what it may not.
//
// This guard is deliberately relay-only. The sync API must keep accepting any
// bytes: a paired device legitimately syncs plugin JavaScript, themes and
// whatever else lives under .obsidian. It is the agent path that needs a
// narrower door, because that is the one an injected instruction can reach.
//
// Two independent checks, because either alone is weak. The extension list
// says what the vault is expected to hold; the content sniff says what the
// bytes actually are. An executable renamed to .png fails the second, and a
// legitimate-looking format nobody wants in a vault fails the first.

// Extensions Obsidian embeds and renders. Text belongs in write_note, so it is
// not here — an agent reaching for .md through this tool has the wrong tool.
var allowedAttachmentExt = map[string]bool{
	// images
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true,
	"avif": true, "bmp": true, "svg": true,
	// documents
	"pdf": true,
	// audio
	"mp3": true, "wav": true, "m4a": true, "ogg": true, "opus": true,
	"flac": true, "3gp": true,
	// video
	"mp4": true, "mov": true, "webm": true, "ogv": true, "mkv": true,
}

// Executable and loadable-object signatures, refused whatever the extension
// claims. Not an antivirus — the point is that a vault has no reason to hold
// any of these, so refusing them costs nothing and closes the obvious trick.
var executableMagic = []struct {
	prefix []byte
	what   string
}{
	{[]byte{0x7f, 'E', 'L', 'F'}, "an ELF executable"},
	{[]byte{'M', 'Z'}, "a DOS/Windows executable"},
	{[]byte{0xfe, 0xed, 0xfa, 0xce}, "a Mach-O executable"},
	{[]byte{0xfe, 0xed, 0xfa, 0xcf}, "a Mach-O executable"},
	{[]byte{0xce, 0xfa, 0xed, 0xfe}, "a Mach-O executable"},
	{[]byte{0xcf, 0xfa, 0xed, 0xfe}, "a Mach-O executable"},
	{[]byte{0xca, 0xfe, 0xba, 0xbe}, "a Mach-O fat binary or Java class file"},
	{[]byte{'#', '!'}, "a script with a shebang"},
	{[]byte{'d', 'e', 'x', '\n'}, "an Android dex file"},
	{[]byte{0x00, 0x61, 0x73, 0x6d}, "a WebAssembly module"},
}

// Signatures for the formats worth pinning, so bytes and extension have to
// agree. Only formats with a stable, unambiguous magic are listed: a missing
// entry means "no opinion", never "anything goes".
var extMagic = map[string][][]byte{
	"png":  {{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}},
	"jpg":  {{0xff, 0xd8, 0xff}},
	"jpeg": {{0xff, 0xd8, 0xff}},
	"gif":  {[]byte("GIF87a"), []byte("GIF89a")},
	"pdf":  {[]byte("%PDF-")},
	"flac": {[]byte("fLaC")},
	"ogg":  {[]byte("OggS")},
	"opus": {[]byte("OggS")},
	"mp3":  {[]byte("ID3"), {0xff, 0xfb}, {0xff, 0xf3}, {0xff, 0xf2}},
	"wav":  {[]byte("RIFF")},
	"webp": {[]byte("RIFF")},
	"avif": {[]byte("ftyp")}, // checked at offset 4, see below
	"mp4":  {[]byte("ftyp")},
	"mov":  {[]byte("ftyp")},
	"webm": {{0x1a, 0x45, 0xdf, 0xa3}},
	"mkv":  {{0x1a, 0x45, 0xdf, 0xa3}},
}

// ISO base-media formats carry their magic at offset 4, not 0.
var magicAtOffset4 = map[string]bool{"mp4": true, "mov": true, "avif": true}

// SVG is XML that Obsidian renders, so a script in one runs where the note is
// displayed. Refusing active content is the difference between an image format
// and a delivery mechanism.
var svgActiveContent = regexp.MustCompile(`(?is)<script|javascript:|\son\w+\s*=|<foreignObject`)

// guardAttachment decides whether these bytes may be written at this path.
// Returns nil to allow, or an error naming the reason in terms the caller can
// act on.
func guardAttachment(p string, body []byte) error {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	if ext == "" {
		return fmt.Errorf("refusing %s: an attachment needs a file extension, so the vault knows what it is", p)
	}
	if !allowedAttachmentExt[ext] {
		return fmt.Errorf(
			"refusing %s: .%s is not an attachment type this vault accepts. Allowed: %s. Text notes go through write_note",
			p, ext, allowedExtList())
	}

	for _, sig := range executableMagic {
		if bytes.HasPrefix(body, sig.prefix) {
			return fmt.Errorf("refusing %s: the content is %s, whatever the extension says", p, sig.what)
		}
	}

	if ext == "svg" && svgActiveContent.Match(body) {
		return fmt.Errorf(
			"refusing %s: the SVG carries active content (a script, an event handler, or a foreignObject). "+
				"Obsidian renders SVG where the note is displayed", p)
	}

	if sigs, pinned := extMagic[ext]; pinned {
		offset := 0
		if magicAtOffset4[ext] {
			offset = 4
		}
		if !matchesAny(body, sigs, offset) {
			return fmt.Errorf(
				"refusing %s: the content does not look like a .%s file. Name it for what it actually is", p, ext)
		}
	}
	return nil
}

func matchesAny(body []byte, sigs [][]byte, offset int) bool {
	if len(body) <= offset {
		return false
	}
	rest := body[offset:]
	for _, sig := range sigs {
		if bytes.HasPrefix(rest, sig) {
			return true
		}
	}
	return false
}

func allowedExtList() string {
	out := make([]string, 0, len(allowedAttachmentExt))
	for ext := range allowedAttachmentExt {
		out = append(out, ext)
	}
	sortStrings(out)
	return strings.Join(out, ", ")
}

// sortStrings keeps the error message stable across runs; map order is not.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
