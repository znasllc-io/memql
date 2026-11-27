package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Authority changes serialize on the same transaction connection as the graph
// append. A process dying loses both the lock and its uncommitted changes.
// This is shared by ordinary identity writes and atomic account management;
// guarding only the UI or the role-assignment builtin would leave other writers.
const accountAuthorityLock = `SELECT pg_advisory_xact_lock(1297173610, 0)`

type accountAuthorityMutationKey struct{}
type accountAuthorityMutation struct{ primaryOwner string }

func accountAuthorityConcept(concept string) bool {
	return concept == conceptIdentityUser || concept == "v1:accounts:account" || concept == conceptIdentityGroupMembership || concept == conceptIdentityGroup
}

// staged-data: MUST-NOT-GATE -- authorization evidence is never a public read.
func authorityRows(ctx context.Context, db bun.IDB, concept string) ([]memorynodes.MemoryNode, error) {
	var rows []memorynodes.MemoryNode
	err := db.NewSelect().Model(&rows).DistinctOn("id").Where("concept = ?", concept).
		OrderExpr(`id ASC, "createdAt" DESC`).Scan(ctx)
	return rows, err
}

func authorityRow(ctx context.Context, db bun.IDB, concept, id string) (memorynodes.MemoryNode, bool, error) {
	var rows []memorynodes.MemoryNode
	bare := BareShortId(id)
	err := db.NewSelect().Model(&rows).Where("concept = ?", concept).
		Where("id IN (?)", bun.In([]string{bare, concept + ":" + bare})).
		OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx)
	if err != nil || len(rows) == 0 {
		return memorynodes.MemoryNode{}, false, err
	}
	return rows[0], true, nil
}

func activeAuthorityUser(payload map[string]any) bool {
	return boolFromAny(payload["active"]) && !boolFromAny(payload["deleted"])
}

func activePrimaryOwners(ctx context.Context, db bun.IDB) ([]string, error) {
	rows, err := authorityRows(ctx, db, conceptIdentityUser)
	if err != nil {
		return nil, err
	}
	var owners []string
	for _, row := range rows {
		p := accountRowPayload(row)
		if activeAuthorityUser(p) && stringFromAny(p["role"]) == "owner" {
			owners = append(owners, BareShortId(row.ID))
		}
	}
	return owners, nil
}

func (s *bunStore) insertAuthorityNode(ctx context.Context, node *memorynodes.MemoryNode) error {
	if db, ok := s.db.(*bun.DB); ok {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return db.RunInTx(bounded, nil, func(ctx context.Context, tx bun.Tx) error {
			return (&bunStore{db: tx, onInsert: s.onInsert}).insertAuthorityNode(ctx, node)
		})
	}
	if _, err := s.db.ExecContext(ctx, accountAuthorityLock); err != nil {
		return err
	}
	if err := validateAccountAuthorityAppend(ctx, s.db, node); err != nil {
		return err
	}
	return s.insertMemoryNodeRow(ctx, node)
}

func validateAccountAuthorityAppend(ctx context.Context, db bun.IDB, node *memorynodes.MemoryNode) error {
	var next map[string]any
	if err := json.Unmarshal(node.Payload, &next); err != nil {
		return err
	}
	prior, found, err := authorityRow(ctx, db, node.Concept, node.ID)
	if err != nil {
		return err
	}
	if found && !node.CreatedAt.After(prior.CreatedAt) {
		return errors.New("account_access_changed: read the current record before changing access")
	}
	before := accountRowPayload(prior)
	change, authorized := ctx.Value(accountAuthorityMutationKey{}).(accountAuthorityMutation)
	// Raw insert/update can bypass a named mutation's @serverOnly gate.
	// Group administration must reach its governed action even for operators;
	// trusted invitation/domain/role-test lifecycle writers retain their path.
	if (node.Concept == conceptIdentityGroup || node.Concept == conceptIdentityGroupMembership) && !authorized && !auth.OriginFromContext(ctx).IsInternal() {
		return errors.New("account_access_managed: change groups and membership through their management actions")
	}
	switch node.Concept {
	case conceptIdentityUser:
		// Role changes retire previously minted JWT authority, including an
		// owner's open streams. Refresh resolves the new role and epoch.
		if found && stringFromAny(before["role"]) != stringFromAny(next["role"]) {
			priorEpoch, _ := intWithOkFromAny(before["revocationEpoch"])
			nextEpoch, _ := intWithOkFromAny(next["revocationEpoch"])
			if nextEpoch <= priorEpoch {
				next["revocationEpoch"] = priorEpoch + 1
			}
			node.Payload, err = json.Marshal(next)
			if err != nil {
				return err
			}
		}
		isOwner := activeAuthorityUser(next) && stringFromAny(next["role"]) == "owner"
		wasOwner := found && activeAuthorityUser(before) && stringFromAny(before["role"]) == "owner"
		if isOwner && !(authorized && change.primaryOwner == BareShortId(node.ID)) {
			// Ordinary user writes cannot introduce an owner. Avoid scanning
			// the directory on login/profile updates and batched user imports.
			owners, err := activePrimaryOwners(ctx, db)
			if err != nil {
				return err
			}
			for _, owner := range owners {
				if owner != BareShortId(node.ID) {
					return errors.New("account_single_owner: transfer ownership instead of adding another owner")
				}
			}
		}
		if wasOwner && !isOwner && !(authorized && change.primaryOwner != "") {
			return errors.New("account_owner_required: transfer ownership before changing the owner's role or status")
		}
		if found && activeAuthorityUser(before) && !activeAuthorityUser(next) {
			accounts, err := authorityRows(ctx, db, "v1:accounts:account")
			if err != nil {
				return err
			}
			for _, account := range accounts {
				p := accountRowPayload(account)
				if stringFromAny(p["status"]) == "active" && sameRowAuthzOwner(stringFromAny(p["ownerUserId"]), BareShortId(node.ID)) {
					return errors.New("account_owner_required: transfer the person's active accounts before deactivating them")
				}
			}
		}
	case "v1:accounts:account":
		owner := BareShortId(stringFromAny(next["ownerUserId"]))
		oldOwner := BareShortId(stringFromAny(before["ownerUserId"]))
		if oldOwner != "" && owner != oldOwner && !authorized {
			return errors.New("account_owner_transfer_required: use the account ownership transfer")
		}
		if !authorized {
			for _, field := range []string{"allowedApps", "accessRevision"} {
				if !reflect.DeepEqual(next[field], before[field]) {
					return fmt.Errorf("account_access_managed: change %s through account access management", field)
				}
			}
		}
		if owner == "" && stringFromAny(next["status"]) == "active" {
			// The self seed precedes the first ownership ceremony. Existing
			// unowned rows are repaired only from one unambiguous cluster owner.
			owners, err := activePrimaryOwners(ctx, db)
			if err != nil {
				return err
			}
			if len(owners) == 1 {
				next["ownerUserId"] = conceptIdentityUser + ":" + owners[0]
				owner = owners[0]
				node.Payload, err = json.Marshal(next)
				if err != nil {
					return err
				}
			}
			if owner == "" && (BareShortId(node.ID) != "self" || len(owners) > 0) {
				return errors.New("account_owner_required: name one active owner for this account")
			}
		}
		if owner != "" && stringFromAny(next["status"]) == "active" {
			user, exists, err := authorityRow(ctx, db, conceptIdentityUser, owner)
			if err != nil {
				return err
			}
			if !exists || !activeAuthorityUser(accountRowPayload(user)) {
				return errors.New("account_owner_required: choose an active person as the account owner")
			}
			if BareShortId(node.ID) == "self" && stringFromAny(accountRowPayload(user)["role"]) != "owner" {
				return errors.New("account_owner_required: the primary account belongs to the cluster owner")
			}
		}
	case conceptIdentityGroup:
		if !authorized && !reflect.DeepEqual(next["allowedApps"], before["allowedApps"]) {
			return errors.New("account_access_managed: use account access management to assign group apps")
		}
	case conceptIdentityGroupMembership:
		if found {
			for _, field := range []string{"groupId", "userId"} {
				if BareShortId(stringFromAny(next[field])) != BareShortId(stringFromAny(before[field])) {
					return errors.New("account_membership_immutable: remove the membership before adding a different person or group")
				}
			}
		}
		groupID := BareShortId(stringFromAny(next["groupId"]))
		if strings.HasPrefix(groupID, "acct-") {
			account, exists, err := authorityRow(ctx, db, "v1:accounts:account", strings.TrimPrefix(groupID, "acct-"))
			if err != nil {
				return err
			}
			if exists && stringFromAny(next["status"]) != "active" {
				p := accountRowPayload(account)
				if stringFromAny(p["status"]) == "active" && sameRowAuthzOwner(stringFromAny(p["ownerUserId"]), stringFromAny(next["userId"])) {
					return errors.New("account_owner_required: transfer ownership before removing the owner")
				}
			}
		}
		if role := auth.AccountRole(stringFromAny(next["organizationRole"])); role != "" && (!auth.ValidAccountRole(role) || role == auth.AccountOwner) {
			return errors.New("account_role_invalid: ownership is derived from the account's single owner")
		}
		if !authorized {
			for _, field := range []string{"organizationRole", "allowedApps"} {
				if !reflect.DeepEqual(next[field], before[field]) {
					return errors.New("account_access_managed: use account membership management to change roles or apps")
				}
			}
		}
	}
	return nil
}
