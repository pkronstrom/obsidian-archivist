package vault

import (
	"path"
	"strings"
)

// ConfigDir is the Obsidian configuration directory this server understands.
//
// Obsidian lets a user rename it. This server does not follow that, and the
// plugin refuses to enable config sync when it has been renamed, rather than
// translating paths on the way through: a translation step is exactly what
// makes config sync stop landing where Obsidian looks.
const ConfigDir = ".obsidian"

// ConfigSyncable reports whether a path inside the config directory may sync.
//
// An ALLOWLIST, never a denylist, because the failure modes are asymmetric: a
// missed allowlist entry means a theme does not travel, and a missed denylist
// entry means a credential sits in git history permanently. A config file we
// have never heard of therefore does not sync, which is the safe direction.
//
// Obsidian's config FILE NAMES have been stable for years and whole files are
// synced, so schema drift inside them does not matter here.
func ConfigSyncable(rel string) bool {
	rest, ok := strings.CutPrefix(rel, ConfigDir+"/")
	if !ok || rest == "" {
		return false
	}
	// No traversal, no nested dotfiles, no temp droppings.
	if !isCleanConfigPath(rest) {
		return false
	}

	switch rest {
	case "app.json", "appearance.json", "hotkeys.json",
		"core-plugins.json", "community-plugins.json":
		return true
	}

	switch {
	// Snippets are CSS the user wrote. Anything else parked in that folder is
	// not a snippet and does not travel.
	case strings.HasPrefix(rest, "snippets/"):
		return strings.HasSuffix(rest, ".css") &&
			!strings.Contains(strings.TrimPrefix(rest, "snippets/"), "/")

	// A theme is a directory of CSS and its manifest, and it may nest assets.
	// Themes are style, not executable code -- unlike plugins, which is why
	// these travel and plugin code does not.
	case strings.HasPrefix(rest, "themes/"):
		return strings.Count(rest, "/") >= 2

	// Only ever data.json, and never Archivist's own. Plugin CODE is
	// deliberately absent: a realistic 15-plugin set is several MB, and every
	// plugin update rewrites main.js wholesale as a fresh blob -- roughly 50 MB
	// a year of unrecoverable churn, which would make plugin updates the
	// dominant driver of vault growth.
	case strings.HasPrefix(rest, "plugins/"):
		parts := strings.Split(rest, "/")
		if len(parts) != 3 || parts[2] != "data.json" {
			return false
		}
		return !isArchivistPlugin(parts[1])
	}

	return false
}

// isArchivistPlugin matches this plugin's own directory, under any of the ids
// it has shipped or might ship under.
//
// This is a HARD exclusion with no override, not a default. The plugin persists
// its settings with saveData, so .obsidian/plugins/archivist/data.json holds
// the server URL and the bearer token. Under a generic per-plugin opt-in,
// ticking Archivist would commit the server's own credential into the vault it
// protects, sync it to every device, and place it in git history permanently --
// recoverable only by noticing and then pruning.
//
// A scanner would very likely catch a key called "token". Relying on that is
// the mistake: this one is knowable in advance, so it is excluded by name.
// A sync tool must never be able to sync its own credentials.
func isArchivistPlugin(id string) bool {
	switch strings.ToLower(id) {
	case "archivist", "obsidian-archivist":
		return true
	}
	return false
}

// isCleanConfigPath rejects traversal, absolute forms, empty segments, nested
// dotfiles and our own temp files. Without it, ".obsidian/../../etc/passwd"
// would reach the switch above with a plausible-looking suffix. ValidPath
// catches that too, but this predicate is consulted from places that do not
// call it first, so it defends itself.
func isCleanConfigPath(rest string) bool {
	if path.Clean(rest) != rest {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, tmpPrefix) {
			return false
		}
	}
	return true
}

// MergeExempt reports whether a path must never be text-merged.
//
// Archivist merges text with diff3, and a text-merged settings file can come
// out syntactically valid and semantically wrong -- the worst failure shape,
// because nothing reports it and Obsidian simply behaves oddly later. So
// allowlisted JSON goes through a key-level merge instead, and on a genuine
// key-level divergence the last writer wins with a conflict copy kept beside it.
//
// CSS snippets and theme stylesheets are ordinary text and keep the ordinary
// merge: conflict markers in CSS are visible, localised and harmless.
func MergeExempt(rel string) bool {
	return ConfigSyncable(rel) && strings.HasSuffix(rel, ".json")
}
