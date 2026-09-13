package groups

// capabilities.go -- the six verbs (epic memql#5165, section D).
//
// Each one runs the same three beats: resolve the caller, run the guards
// against the caller's own authority, then write through store.go under the
// engine's identity. Nothing writes before every guard has passed, so a
// refusal never leaves half a change behind.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// resultConcept is the synthetic concept a capability's answer rides on. Never
// persisted -- the builtin call reads `.First().payload.<key>`.
const resultConcept = "integration:groups:result"

// Integration is the plug-in.
type Integration struct {
	store *Store
	now   func() time.Time
	// warn reports a failure that must not fail the operation -- an audit
	// write that did not land after the row already moved. A func rather
	// than a *slog.Logger so a test can capture it without a handler.
	warn func(msg string, args ...any)
}

// New builds the integration.
func New(engine Engine, warn func(msg string, args ...any)) *Integration {
	return &Integration{
		store: NewStore(engine),
		now:   func() time.Time { return time.Now().UTC() },
		warn:  warn,
	}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "groups" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "groupCreate",
			Description: "Create a custom group, optionally tied to an account.",
			Handler:     i.handleGroupCreate,
			ArgsSchema: map[string]string{
				"name":        "string (required) -- display name.",
				"description": "string -- what the group is for.",
				"accountId":   "string -- the account this group grants; omit for a group that grants nothing.",
			},
		},
		{
			Name:        "groupUpdate",
			Description: "Rename a group or change its description.",
			Handler:     i.handleGroupUpdate,
			ArgsSchema: map[string]string{
				"groupId":     "string (required)",
				"name":        "string -- new display name; omit to leave it.",
				"description": "string -- new description; omit to leave it.",
			},
		},
		{
			Name:        "groupArchive",
			Description: "Archive a group and remove its memberships.",
			Handler:     i.handleGroupArchive,
			ArgsSchema:  map[string]string{"groupId": "string (required)"},
		},
		{
			Name:        "groupMemberAdd",
			Description: "Place a person in a group.",
			Handler:     i.handleGroupMemberAdd,
			ArgsSchema: map[string]string{
				"groupId": "string (required)",
				"userId":  "string (required) -- must rank strictly below the caller, and must not be the caller.",
			},
		},
		{
			Name:        "groupMemberRemove",
			Description: "Remove a person from a group.",
			Handler:     i.handleGroupMemberRemove,
			ArgsSchema: map[string]string{
				"groupId": "string (required)",
				"userId":  "string (required) -- the caller themselves, or somebody ranking strictly below them.",
			},
		},
		{
			Name:        "groupEnsureForAccount",
			Description: "Ensure the account-kind group for one account exists. Idempotent.",
			Handler:     i.handleGroupEnsureForAccount,
			ArgsSchema:  map[string]string{"accountId": "string (required)"},
		},
		{
			Name:        "groupArchiveForAccount",
			Description: "Archive every group tied to one account and remove their memberships.",
			Handler:     i.handleGroupArchiveForAccount,
			ArgsSchema:  map[string]string{"accountId": "string (required)"},
		},
	}
}

// ---------------------------------------------------------------------
// The verbs

func (i *Integration) handleGroupCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c, err := resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.requireCapability(ctx, auth.VerbCreate); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(asString(args["name"]))
	if name == "" {
		return nil, refusal(CodeGroupNotFound, "a group needs a name")
	}
	accountID := memql.BareShortId(strings.TrimSpace(asString(args["accountId"])))
	if accountID != "" {
		if err := i.requireActiveAccount(ctx, accountID); err != nil {
			return nil, err
		}
	}

	g := Group{
		ID:          id.NewShortId(),
		Name:        name,
		Description: strings.TrimSpace(asString(args["description"])),
		// ALWAYS custom. The account-kind group is written by
		// groupEnsureForAccount at a derived id, and letting a caller mint a
		// second one would split an account's membership across two rows
		// with no way to tell which one grants.
		Kind:      KindCustom,
		AccountID: accountID,
		Status:    StatusActive,
	}
	if err := i.store.WriteGroup(ctx, g); err != nil {
		return nil, err
	}
	i.log(ctx, c, "group_created", g.ID, map[string]any{
		"name": g.Name, "accountId": g.AccountID, "kind": g.Kind,
	})
	return i.node("groupCreate", map[string]any{
		"groupId": g.ID, "name": g.Name, "accountId": g.AccountID, "kind": g.Kind, "status": g.Status,
	})
}

func (i *Integration) handleGroupUpdate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c, err := resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.requireCapability(ctx, auth.VerbUpdate); err != nil {
		return nil, err
	}
	g, err := i.requireActiveGroup(ctx, asString(args["groupId"]))
	if err != nil {
		return nil, err
	}
	// An omitted argument leaves the field, which is what makes this a
	// rename rather than a restatement. The read-merge happens HERE rather
	// than in the mutation, because the mutation takes the whole row.
	if name, present := args["name"]; present && strings.TrimSpace(asString(name)) != "" {
		g.Name = strings.TrimSpace(asString(name))
	}
	if desc, present := args["description"]; present {
		g.Description = strings.TrimSpace(asString(desc))
	}
	if err := i.store.WriteGroup(ctx, *g); err != nil {
		return nil, err
	}
	i.log(ctx, c, "group_updated", g.ID, map[string]any{"name": g.Name})
	return i.node("groupUpdate", map[string]any{
		"groupId": g.ID, "name": g.Name, "description": g.Description,
	})
}

func (i *Integration) handleGroupArchive(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c, err := resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.requireCapability(ctx, auth.VerbUpdate); err != nil {
		return nil, err
	}
	g, err := i.requireActiveGroup(ctx, asString(args["groupId"]))
	if err != nil {
		return nil, err
	}
	// An ACCOUNT-KIND group belongs to its account (D5). Archiving it while
	// the account is live would leave the account with no way for its people
	// to reach its work, while every screen still shows it configured --
	// archive the ACCOUNT, and the cascade takes this with it.
	if g.Kind == KindAccount && g.AccountID != "" {
		status, err := i.store.AccountStatus(ctx, g.AccountID)
		if err != nil {
			return nil, err
		}
		if status == StatusActive {
			return nil, refusal(CodeGroupAccountActive,
				fmt.Sprintf("%s is the group of account %s, which is still active. Archive the account instead; "+
					"its groups and their memberships follow", g.ID, g.AccountID))
		}
	}
	removed, err := i.archiveGroup(ctx, *g, c.userID)
	if err != nil {
		return nil, err
	}
	i.log(ctx, c, "group_archived", g.ID, map[string]any{"membershipsRemoved": removed})
	return i.node("groupArchive", map[string]any{
		"groupId": g.ID, "status": StatusArchived, "membershipsRemoved": removed,
	})
}

func (i *Integration) handleGroupMemberAdd(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c, err := resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.requireCapability(ctx, auth.VerbUpdate); err != nil {
		return nil, err
	}
	g, err := i.requireActiveGroup(ctx, asString(args["groupId"]))
	if err != nil {
		return nil, err
	}
	target := memql.BareShortId(strings.TrimSpace(asString(args["userId"])))
	if target == "" {
		return nil, refusal(CodeTargetUserNotFound, "a membership needs a user id")
	}
	// NOBODY ADDS THEMSELVES (D7). Checked before the rank rule, because
	// self-add would otherwise be refused with "does not rank below your own"
	// -- true, and a confusing way to say "not this way".
	if sameUser(target, c.userID) {
		return nil, refusal(CodeSelfAddRefused,
			"a person may not add themselves to a group -- ask somebody who outranks you to place you")
	}
	role, err := i.store.UserRole(ctx, target)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, refusal(CodeTargetUserNotFound, fmt.Sprintf("%s names no user on this cluster", target))
	}
	if err := c.requireTargetBelow(target, role); err != nil {
		return nil, err
	}

	m := Membership{
		ID:      MembershipID(g.ID, target),
		GroupID: g.ID,
		UserID:  target,
		Origin:  OriginAdded,
		Status:  StatusActive,
	}
	if err := i.store.WriteMembership(ctx, m, c.userID, ""); err != nil {
		return nil, err
	}
	i.log(ctx, c, "group_member_added", m.ID, map[string]any{
		"groupId": g.ID, "userId": target, "origin": OriginAdded,
	})
	return i.node("groupMemberAdd", map[string]any{
		"membershipId": m.ID, "groupId": g.ID, "userId": target, "origin": m.Origin, "status": m.Status,
	})
}

func (i *Integration) handleGroupMemberRemove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c, err := resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	target := memql.BareShortId(strings.TrimSpace(asString(args["userId"])))
	if target == "" {
		return nil, refusal(CodeTargetUserNotFound, "a membership needs a user id")
	}
	self := sameUser(target, c.userID)
	// A person may ALWAYS remove themselves, and needs no capability to do
	// it. Leaving a group grants nobody anything, and the alternative is
	// somebody who cannot get out of a group they were placed in.
	if !self {
		if err := c.requireCapability(ctx, auth.VerbUpdate); err != nil {
			return nil, err
		}
		role, err := i.store.UserRole(ctx, target)
		if err != nil {
			return nil, err
		}
		if role == "" {
			return nil, refusal(CodeTargetUserNotFound, fmt.Sprintf("%s names no user on this cluster", target))
		}
		if err := c.requireTargetBelow(target, role); err != nil {
			return nil, err
		}
	}
	// The group is read but NOT required to be active: removing somebody
	// from an archived group is tidying history, and refusing it would
	// strand memberships nobody can clear.
	groupID := memql.BareShortId(strings.TrimSpace(asString(args["groupId"])))
	g, err := i.store.GroupByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, refusal(CodeGroupNotFound, fmt.Sprintf("%s names no group", groupID))
	}

	m := Membership{
		ID:      MembershipID(g.ID, target),
		GroupID: g.ID,
		UserID:  target,
		Origin:  OriginAdded,
		Status:  StatusRemoved,
	}
	if err := i.store.WriteMembership(ctx, m, "", c.userID); err != nil {
		return nil, err
	}
	i.log(ctx, c, "group_member_removed", m.ID, map[string]any{
		"groupId": g.ID, "userId": target, "self": self,
	})
	return i.node("groupMemberRemove", map[string]any{
		"membershipId": m.ID, "groupId": g.ID, "userId": target, "status": StatusRemoved,
	})
}

// ---------------------------------------------------------------------
// Shared helpers

func (i *Integration) requireActiveGroup(ctx context.Context, rawID string) (*Group, error) {
	groupID := memql.BareShortId(strings.TrimSpace(rawID))
	if groupID == "" {
		return nil, refusal(CodeGroupNotFound, "a group id is required")
	}
	g, err := i.store.GroupByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, refusal(CodeGroupNotFound, fmt.Sprintf("%s names no group", groupID))
	}
	if g.Status != StatusActive {
		return nil, refusal(CodeGroupNotActive,
			fmt.Sprintf("%s is %s. An archived group grants nothing, and editing one reads as reviving it", g.ID, g.Status))
	}
	return g, nil
}

func (i *Integration) requireActiveAccount(ctx context.Context, accountID string) error {
	status, err := i.store.AccountStatus(ctx, accountID)
	if err != nil {
		return err
	}
	if status == "" {
		return refusal(CodeAccountNotFound, fmt.Sprintf("%s names no account", accountID))
	}
	if status != StatusActive {
		return refusal(CodeAccountNotActive,
			fmt.Sprintf("account %s is %s, and a group tied to it would grant nothing", accountID, status))
	}
	return nil
}

// archiveGroup archives one group and removes every active membership on it.
//
// Returns the count removed, which the archive cascade reports too -- an
// operator archiving an account wants to know how many people that moved.
func (i *Integration) archiveGroup(ctx context.Context, g Group, actingUserID string) (int, error) {
	members, err := i.store.MembersOfGroup(ctx, g.ID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, m := range members {
		if m.Status != StatusActive {
			continue
		}
		m.Status = StatusRemoved
		if err := i.store.WriteMembership(ctx, m, "", actingUserID); err != nil {
			return removed, err
		}
		removed++
	}
	g.Status = StatusArchived
	if err := i.store.WriteGroup(ctx, g); err != nil {
		return removed, err
	}
	return removed, nil
}

// MembershipID derives the one id a (group, user) pair ever has (D11).
//
// DERIVED rather than minted, which is what makes re-adding somebody a new
// VERSION of one logical row instead of a second row. History is the versions,
// and a second row would make "is this person a member" a question with two
// answers.
func MembershipID(groupID, userID string) string {
	return memql.BareShortId(groupID) + "-" + memql.BareShortId(userID)
}

// sameUser compares two user ids across both spellings.
func sameUser(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || memql.BareShortId(a) == memql.BareShortId(b)
}

func (i *Integration) node(suffix string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("groups: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "groups:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: i.now(),
		Payload:   encoded,
	}}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
