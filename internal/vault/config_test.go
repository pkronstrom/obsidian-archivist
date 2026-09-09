package vault

import "testing"

func TestHiddenSyncPolicy(t *testing.T) {
	for _, p := range []string{
		".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json",
		".archivist/plugin-inventory/00000000-0000-0000-0000-000000000000.json",
	} {
		if Skip(p) {
			t.Errorf("inventory refused: %s", p)
		}
	}
	for _, p := range []string{
		".obsidian/app.json", ".obsidian/appearance.json", ".obsidian/hotkeys.json",
		".obsidian/core-plugins.json", ".obsidian/community-plugins.json",
		".obsidian/snippets/dark.css", ".obsidian/themes/Minimal/theme.css",
		".obsidian/plugins/dataview/data.json",
		".archivist", ".archivist/plugin-inventory", ".archivist/secret.json",
		".archivist/plugin-inventory/not-a-uuid.json",
		".archivist/plugin-inventory/01234567-89AB-cdef-0123-456789abcdef.json",
		".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.local.json",
		".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.conflict.json",
		".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json/child",
		"folder/.archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json",
		".archivist/plugin-inventory/../plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json",
		".archivist/plugin-inventory/.archivist-tmp-123",
	} {
		if !Skip(p) {
			t.Errorf("excluded path admitted: %s", p)
		}
	}
}
