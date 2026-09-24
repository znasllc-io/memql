package groups

import (
	"context"
	"sort"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// groupPeople projects only an already-authorized organization's roster. A
// delegated admin never scans global users or looks up arbitrary identities.
func (i *Integration) handleGroupPeople(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c, err := resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	g, err := i.requireActiveGroup(ctx, asString(args["groupId"]))
	if err != nil {
		return nil, err
	}
	if err := i.requireOrganizationCapability(ctx, c, g.AccountID, auth.VerbUpdate); err != nil {
		return nil, err
	}
	if g.AccountID == "" {
		return nil, nil
	}
	groups, err := i.store.GroupsForAccount(ctx, g.AccountID)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, group := range groups {
		if group.Status != StatusActive {
			continue
		}
		members, err := i.store.MembersOfGroup(ctx, group.ID)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			if member.Status == StatusActive {
				ids[memql.BareShortId(member.UserID)] = true
			}
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		if id != "" {
			ordered = append(ordered, id)
		}
	}
	sort.Strings(ordered)
	if len(ordered) > 100 {
		ordered = ordered[:100]
	}
	var out []memorynodes.MemoryNode
	for _, id := range ordered {
		row, err := i.store.PersonForOrganizationManagement(ctx, id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			continue
		}
		if active, ok := row["active"].(bool); !ok || !active {
			continue
		}
		person := map[string]any{"id": id, "active": true, "status": "active"}
		for _, field := range []string{"displayName", "firstName", "lastName", "primaryEmail", "role"} {
			person[field] = rowString(row, field)
		}
		nodes, err := i.node("groupPeople", person)
		if err != nil {
			return nil, err
		}
		nodes[0].ID = id
		out = append(out, nodes...)
	}
	return out, nil
}
