package memql

// WHO A PERSON MAY SHARE A LIBRARY FILE WITH (file sharing, owner-approved
// 2026-11-18).
//
// Fleet's directory, narrowed. A person sees the people of their own
// ORGANIZATION -- an active membership in an active group of kind `account`
// that the caller is also an active member of -- and a holder of `read
// principal` (owner, developer and admin by seed) sees everyone, because they
// already read every person in Users. Custom groups do not count here, unlike
// Fleet: a file is shared within the organization a person works in, and a
// custom group can cross organizations.
//
// BY DISPLAY NAME ONLY, for every caller. Fleet hands an email to a caller who
// already reads one; a share dialog has no use for it, so this never does.
//
// libraryShareFile asks this same builtin before it writes, so the write is
// held to exactly what the dialog offered (integrations/library/share.go).

import (
	"context"
	"encoding/json"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// LibraryShareDirectoryConcept names the virtual rows libraryShareDirectory
// answers with. Nothing is persisted.
const LibraryShareDirectoryConcept = "v1:library:shareDirectory"

// evaluateLibraryShareDirectoryExpression serves `libraryShareDirectory`: one
// row per person the caller may pick, {userId, displayName}, sorted by name,
// never the caller.
func (e *MemQLEngine) evaluateLibraryShareDirectoryExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if actingUserFromContext(ctx) == "" {
		return nil, fmt.Errorf("libraryShareDirectory: sign in to see who you can share a file with")
	}
	dir, err := e.shareDirectoryFor(ctx, true)
	if err != nil {
		return nil, err
	}
	rows := make([]memorynodes.MemoryNode, 0, len(dir.people))
	for _, p := range dir.people {
		raw, err := json.Marshal(map[string]any{"userId": p.Id, "displayName": p.Name})
		if err != nil {
			return nil, err
		}
		rows = append(rows, memorynodes.MemoryNode{
			ID: p.Id, Concept: LibraryShareDirectoryConcept, Type: memorynodes.NodeTypeObject, Payload: raw,
		})
	}
	return rows, nil
}
