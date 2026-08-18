package vault

import "testing"

func TestConfigAllowlist(t *testing.T) {
	allowed := []string{
		".obsidian/app.json",
		".obsidian/appearance.json",
		".obsidian/hotkeys.json",
		".obsidian/core-plugins.json",
		".obsidian/community-plugins.json",
		".obsidian/snippets/dark-tables.css",
		".obsidian/themes/Minimal/theme.css",
		".obsidian/themes/Minimal/manifest.json",
		".obsidian/plugins/dataview/data.json",
	}
	for _, p := range allowed {
		if !ConfigSyncable(p) {
			t.Errorf("ConfigSyncable(%q) = false, want true", p)
		}
	}

	refused := []string{
		".obsidian/workspace.json",
		".obsidian/workspace-mobile.json",
		".obsidian/graph.json",
		".obsidian/plugins/dataview/main.js",
		".obsidian/plugins/dataview/manifest.json",
		".obsidian/plugins/dataview/styles.css",
		".obsidian/snippets/notes.md",
		".obsidian/types.json",
		".obsidian/plugins/dataview/cache/index.db",
		".smart-env/config.json",
		".trash/deleted.md",
		"notes/idea.md",
	}
	for _, p := range refused {
		if ConfigSyncable(p) {
			t.Errorf("ConfigSyncable(%q) = true, want false", p)
		}
	}
}

// The one exclusion with no override anywhere: the plugin's own data.json holds
// the bearer token for this very server.
func TestArchivistDataIsNeverSyncable(t *testing.T) {
	for _, p := range []string{
		".obsidian/plugins/archivist/data.json",
		".obsidian/plugins/Archivist/data.json",
		".obsidian/plugins/obsidian-archivist/data.json",
	} {
		if ConfigSyncable(p) {
			t.Errorf("ConfigSyncable(%q) = true; this file holds the server's own credential", p)
		}
	}
}

// A theme directory may nest. A plugin directory may not contribute anything
// but data.json.
func TestThemeSubdirectoriesAreAllowedAndPluginOnesAreNot(t *testing.T) {
	if !ConfigSyncable(".obsidian/themes/Things/assets/logo.svg") {
		t.Error("a nested theme asset should sync")
	}
	if ConfigSyncable(".obsidian/plugins/dataview/lib/vendor.js") {
		t.Error("plugin code must never sync")
	}
}

// An empty or malformed path must not accidentally pass.
func TestDegenerateConfigPaths(t *testing.T) {
	for _, p := range []string{"", ".obsidian", ".obsidian/", ".obsidian/snippets", ".obsidian/plugins"} {
		if ConfigSyncable(p) {
			t.Errorf("ConfigSyncable(%q) = true, want false", p)
		}
	}
}
