package pipelines

import (
	"encoding/hex"
	"strings"
)

// ValidArtifactIntentIDs bounds the cross-node receipt references. This is
// structural validation only; a scoped journal lookup supplies authority and
// immutable object evidence before any artifact is consumed.
func ValidArtifactIntentIDs(ids []string) bool {
	if len(ids) > 1024 {
		return false
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if len(id) != 64 || strings.ToLower(id) != id || seen[id] {
			return false
		}
		if _, err := hex.DecodeString(id); err != nil {
			return false
		}
		seen[id] = true
	}
	return true
}
