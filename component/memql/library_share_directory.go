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
// GROUPS (Nov 20 2026). The groups offered are the organization groups the
// caller is an active member of -- or every active group, custom ones too, for
// a caller who sees everyone (fleet_share_directory.go). A group row carries
// how many active people it holds; asked with `groupId`, the builtin answers
// the people a share with that group would reach instead: its active members
// other than the caller, as person rows, and nothing for a group not offered.
//
// libraryShareFile asks this same builtin before it writes, so the write is
// held to exactly what the dialog offered (integrations/library/share.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// LibraryShareDirectoryConcept names the virtual rows libraryShareDirectory
// answers with. Nothing is persisted.
const LibraryShareDirectoryConcept = "v1:library:shareDirectory"

// evaluateLibraryShareDirectoryExpression serves `libraryShareDirectory`: one
// row per person the caller may pick, {userId, displayName}, sorted by name,
// never the caller, then one row per group, {groupId, displayName, members}.
// With a `groupId`, the people that group's share would reach instead.
func (e *MemQLEngine) evaluateLibraryShareDirectoryExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
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
	people := dir.people
	var groups []shareEntry
	if group := BareShortId(strings.TrimSpace(stringArg(args, "groupId"))); group != "" {
		people = nil
		if dir.offersGroup(group) {
			people = dir.members[group]
		}
	} else {
		groups = dir.groups
	}
	rows := make([]memorynodes.MemoryNode, 0, len(people)+len(groups))
	add := func(id string, payload map[string]any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		rows = append(rows, memorynodes.MemoryNode{
			ID: id, Concept: LibraryShareDirectoryConcept, Type: memorynodes.NodeTypeObject, Payload: raw,
		})
		return nil
	}
	for _, p := range people {
		if err := add(p.Id, map[string]any{"userId": p.Id, "displayName": p.Name}); err != nil {
			return nil, err
		}
	}
	for _, g := range groups {
		// A group's row id is prefixed so a person and a group sharing a short
		// id can never collapse into one row in a client's collection.
		if err := add("group:"+g.Id, map[string]any{"groupId": g.Id, "displayName": g.Name, "members": g.Members}); err != nil {
			return nil, err
		}
	}
	return rows, nil
}
