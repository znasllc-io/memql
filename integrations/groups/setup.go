package groups

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func (i *Integration) handleConfigureSelfAccount(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := i.ConfigureSelfAccount(ctx, asString(args["name"]), asString(args["ownerUserId"])); err != nil {
		return nil, err
	}
	return i.node("configureSelfAccount", map[string]any{"accountId": "self", "groupId": AccountGroupID("self")})
}

func (i *Integration) handleGroupEnsureOperatorMembership(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("operator organization membership requires internal origin")
	}
	userID := memql.BareShortId(strings.TrimSpace(asString(args["userId"])))
	if userID == "" {
		return nil, fmt.Errorf("operator membership requires a user")
	}
	role, err := i.store.UserRole(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !auth.IsClusterOperator(auth.Role(role)) {
		return i.node("groupEnsureOperatorMembership", map[string]any{"joined": false})
	}
	account, err := i.store.AccountByID(ctx, "self")
	if err != nil {
		return nil, err
	}
	// A first claim has not named the company yet. Ownership completion
	// ensures the owner synchronously; subsequent boots/events fill operators.
	if account == nil || rowString(account, "status") != StatusActive || rowString(account, "configuredAt") == "" {
		return i.node("groupEnsureOperatorMembership", map[string]any{"joined": false})
	}
	if err := i.ensureSelfMembership(ctx, userID); err != nil {
		return nil, err
	}
	return i.node("groupEnsureOperatorMembership", map[string]any{"joined": true})
}

// ConfigureSelfAccount finishes the organizational part of an ownership claim.
// The caller holds the shared bootstrap lock and has already completed the
// mandatory passkey ceremony. No authorization role or mailbox verification is
// granted here. Every write is resumable at a deterministic existing row id.
func (i *Integration) ConfigureSelfAccount(ctx context.Context, name, ownerUserID string) error {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return fmt.Errorf("organization setup requires internal origin")
	}
	name = strings.TrimSpace(name)
	ownerUserID = memql.BareShortId(strings.TrimSpace(ownerUserID))
	if name == "" || utf8.RuneCountInString(name) > 200 || ownerUserID == "" {
		return fmt.Errorf("organization setup requires a name (at most 200 characters) and an owner")
	}
	role, err := i.store.UserRole(ctx, ownerUserID)
	if err != nil {
		return err
	}
	if role != "owner" {
		return fmt.Errorf("organization setup requires the verified claim owner")
	}
	account, err := i.store.AccountByID(ctx, "self")
	if err != nil {
		return err
	}
	if account == nil {
		if err := i.store.exec(ctx, fmt.Sprintf("mutation createClientAccount(accountId: \"self\", name: %s)", langparser.QuoteString(name))); err != nil {
			return err
		}
	} else if rowString(account, "status") != StatusActive {
		return fmt.Errorf("the cluster organization is archived")
	}
	if account != nil && rowString(account, "ownerUserId") != "" && memql.BareShortId(rowString(account, "ownerUserId")) != ownerUserID {
		return fmt.Errorf("the cluster organization already belongs to a different owner")
	}
	if account == nil || rowString(account, "name") != name || rowString(account, "configuredAt") == "" || memql.BareShortId(rowString(account, "ownerUserId")) != ownerUserID {
		if err := i.store.ConfigureClusterAccount(ctx, name, ownerUserID); err != nil {
			return err
		}
	}
	return i.ensureSelfMembership(ctx, ownerUserID)
}

func (i *Integration) ensureSelfMembership(ctx context.Context, ownerUserID string) error {
	if _, err := i.handleGroupEnsureForAccount(ctx, map[string]any{"accountId": "self"}, 0); err != nil {
		return err
	}
	groupID := AccountGroupID("self")
	members, err := i.store.MembersOfGroup(ctx, groupID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.UserID == ownerUserID && member.Status == StatusActive {
			return nil
		}
	}
	return i.store.WriteMembership(ctx, Membership{
		ID: MembershipID(groupID, ownerUserID), GroupID: groupID, UserID: ownerUserID,
		Origin: OriginAdded, Status: StatusActive,
	}, ownerUserID, "")
}
