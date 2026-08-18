package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type listResult struct {
	Count      int    `json:"count"`
	Total      int    `json:"total"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
	Note       string `json:"note"`
	Notes      []struct {
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Kind  string `json:"kind"`
		Files int    `json:"files"`
	} `json:"notes"`
}

func listCall(t *testing.T, cs *mcp.ClientSession, args map[string]any) listResult {
	t.Helper()
	var out listResult
	if err := json.Unmarshal([]byte(text(call(t, cs, "list_notes", args))), &out); err != nil {
		t.Fatalf("decoding list_notes: %v", err)
	}
	return out
}

// seed writes n notes spread across two folders.
func seed(t *testing.T, cs *mcp.ClientSession, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		folder := "areas"
		if i%2 == 1 {
			folder = "projects/sub"
		}
		call(t, cs, "write_note", map[string]any{
			"path":    fmt.Sprintf("%s/note-%03d.md", folder, i),
			"content": fmt.Sprintf("body %d\n", i),
		})
	}
}

// An unbounded default is the failure this whole change exists to prevent: one
// call must never be able to flood a context window.
func TestListNotesIsCappedByDefault(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 120)

	got := listCall(t, cs, nil)
	if got.Count > 100 {
		t.Fatalf("returned %d entries with no limit given; the cap did not apply", got.Count)
	}
	if got.Total != 120 {
		t.Errorf("Total = %d, want 120", got.Total)
	}
	if !got.HasMore {
		t.Error("HasMore is false on a truncated listing; the caller cannot tell it saw a partial result")
	}
	if got.NextCursor == "" {
		t.Error("no next_cursor on a truncated listing, so the rest is unreachable")
	}
	if got.Note == "" {
		t.Error("no note explaining the truncation")
	}
}

// A cap without a cursor makes data unreachable. Enumeration must complete.
func TestCursorEnumeratesEverythingExactlyOnce(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 120)

	seen := map[string]bool{}
	cursor := ""
	for round := 0; round < 20; round++ {
		args := map[string]any{"limit": 25}
		if cursor != "" {
			args["cursor"] = cursor
		}
		page := listCall(t, cs, args)
		for _, n := range page.Notes {
			if seen[n.Path] {
				t.Fatalf("%s returned twice", n.Path)
			}
			seen[n.Path] = true
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 120 {
		t.Fatalf("enumerated %d of 120 notes", len(seen))
	}
}

// Silently restarting on a bad cursor makes a client loop forever.
func TestInvalidCursorErrorsRatherThanRestarting(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 5)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_notes",
		Arguments: map[string]any{"cursor": "not-base64!!"},
	})
	if err == nil && !res.IsError {
		t.Fatal("an invalid cursor was accepted; a client would restart and loop")
	}
}

func TestLimitIsCappedAtTheMaximum(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 120)

	got := listCall(t, cs, map[string]any{"limit": 100000})
	if got.Count > 500 {
		t.Fatalf("limit=100000 returned %d entries; the server cap did not apply", got.Count)
	}
}

// The cheap path: one folder, with everything below it collapsed to a row.
func TestNonRecursiveListingFoldsSubfolders(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 20)

	got := listCall(t, cs, map[string]any{"recursive": false})
	for _, n := range got.Notes {
		if n.Path == "projects/" {
			if n.Kind != "dir" {
				t.Errorf("projects/ has kind %q, want dir", n.Kind)
			}
			if n.Files == 0 {
				t.Error("projects/ reports no file count")
			}
			return
		}
	}
	t.Fatalf("no folded projects/ row: %+v", got.Notes)
}

// Recursive is the default, so existing callers are unaffected.
func TestRecursiveIsTheDefault(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 10)

	got := listCall(t, cs, nil)
	for _, n := range got.Notes {
		if n.Kind == "dir" {
			t.Fatalf("default listing folded %s into a directory row", n.Path)
		}
	}
}

func TestPrefixNarrowsTheListing(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 20)

	got := listCall(t, cs, map[string]any{"prefix": "areas/"})
	if got.Total == 0 {
		t.Fatal("prefix matched nothing")
	}
	for _, n := range got.Notes {
		if len(n.Path) < 6 || n.Path[:6] != "areas/" {
			t.Errorf("%s is outside the prefix", n.Path)
		}
	}
}

type folderResult struct {
	TotalFiles int   `json:"total_files"`
	TotalBytes int64 `json:"total_bytes"`
	Folders    []struct {
		Path  string `json:"path"`
		Files int    `json:"files"`
		Bytes int64  `json:"bytes"`
	} `json:"folders"`
}

// The map of the vault must cost far less than the listing it replaces.
func TestListFoldersSummarisesWithoutNamingFiles(t *testing.T) {
	cs, _ := session(t)
	seed(t, cs, 60)

	var got folderResult
	raw := text(call(t, cs, "list_folders", nil))
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decoding list_folders: %v", err)
	}
	if got.TotalFiles != 60 {
		t.Errorf("TotalFiles = %d, want 60", got.TotalFiles)
	}
	if len(got.Folders) == 0 {
		t.Fatal("no folders reported")
	}
	if len(got.Folders) >= 60 {
		t.Errorf("%d folder rows for 60 files; this is not a summary", len(got.Folders))
	}
	for _, f := range got.Folders {
		if f.Path[len(f.Path)-1] != '/' {
			t.Errorf("folder %q does not end in /", f.Path)
		}
		if f.Files == 0 {
			t.Errorf("folder %q reports no files", f.Path)
		}
	}
	// No file name may appear: naming them is what makes list_notes expensive.
	if len(raw) > 0 && containsAny(raw, "note-000.md", "note-001.md") {
		t.Error("list_folders leaked file names")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
