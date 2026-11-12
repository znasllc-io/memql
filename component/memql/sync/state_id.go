package sync

import (
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

// stateIDs derives the health row ids. Untracked: a hash-only caller keeps
// no generation history.
var stateIDs = id.NewUntracked()

// StateID is the deterministic id of one domain's health row
// (v1:platform:syncState) for one connector, direction and partition.
//
// It lives on the contract rather than in the runtime so a connector that
// reads its own health -- by id, the one read bounded by its domains and
// partitions -- derives the id the runtime writes under, not a copy of it.
//
// It is derived through core/id from the tuple, not the tuple spelled out:
// the engine's identifier rule refuses a short id with a colon in it, and
// a concept id carries colons. A partition is appended as a FOURTH part and
// an empty one appends nothing, so every row written before partitions
// existed keeps the id it always had.
func StateID(conceptID, connector, direction, partition string) string {
	key := strings.TrimSpace(conceptID) + "|" + strings.TrimSpace(connector) + "|" + strings.TrimSpace(direction)
	if p := strings.TrimSpace(partition); p != "" {
		key += "|" + p
	}
	return string(stateIDs.FromString(key))
}
