package vault

import (
	"regexp"
	"strings"
)

// PluginInventoryDir is the only hidden namespace admitted to ordinary sync.
const PluginInventoryDir = ".archivist/plugin-inventory"

var inventoryPathPattern = regexp.MustCompile(`^\.archivist/plugin-inventory/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json$`)

// PluginInventoryPath admits one whole-file inventory per installation.
// Conflict copies, nested paths and temporary files are deliberately excluded.
func PluginInventoryPath(rel string) bool {
	return inventoryPathPattern.MatchString(rel)
}

// PluginInventoryAncestor identifies the exact directories containing inventories.
func PluginInventoryAncestor(rel string) bool {
	return rel == ".archivist" || rel == PluginInventoryDir
}

// SkipDir applies directory traversal policy, including inventory ancestors.
// Live Obsidian config is permanently excluded; historical blobs remain readable.
func SkipDir(rel string) bool {
	if LocalOnlyDir(rel) {
		return true
	}
	if PluginInventoryAncestor(rel) {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
