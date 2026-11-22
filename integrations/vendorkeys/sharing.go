package vendorkeys

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/worker"
)

// sharing.go -- lending a key (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K6, safety rule S7):
//
//	integration.vendorkeys.shareDirectory  who it can be lent to  (dsl builtin vendorKeyShareDirectory)
//	integration.vendorkeys.setSharing      lend it, or take it back (dsl builtin vendorKeySetSharing)
//
// LENDING IS A MACHINE'S LENDING, with one mode fewer. The key carries the
// sharing block a machine does, the people and groups offered are the ones a
// machine could be lent to (memql.ShareDirectoryFor), and admission on a call
// is the same predicate (worker.Sharing.Admits, resolver.go). The rules below
// are fleetSetSharing's (component/memql/fleet_machine_acts.go), kept in step
// on purpose: one lending model, learned once.
//
// WHAT IS DIFFERENT. There is no way to lend a key to everyone: a key is one
// person's money, and the cluster's own work never spends one. And only the
// key's OWNER lends it. A cluster owner can stop a key (remove it) and cannot
// decide who spends it, so somebody else's key answers here as an unknown id
// does.
//
// WHAT A BORROWER GETS. To pay with the key for their OWN calls, after their
// own key at that vendor and before the cluster's account, when they chose the
// route; under Auto only if the key's owner switched Auto on for it. Nothing
// else: a borrower sees a short card, and cannot check, change, lend or remove
// the key.

// The audit vocabulary for lending.
const (
	auditLent           = "vendor_key_lent"
	auditLendingPaused  = "vendor_key_lending_paused"
	auditLendingResumed = "vendor_key_lending_resumed"
	auditLendingStopped = "vendor_key_lending_stopped"
)

// unknownSubject is ONE SENTENCE for "no such id" and "not yours to pick": a
// write that told them apart would tell anybody with a key who is on the
// cluster, one guess at a time.
func unknownSubject() error {
	return refuse("Some of the people or groups chosen are not ones you can lend this key to. Choose from the people and groups offered.")
}

// handleSetSharing writes who one of the caller's keys is lent to.
func (i *Integration) handleSetSharing(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx); err != nil {
		return nil, err
	}
	userId, err := auth.RequirePersonSession(ctx)
	if err != nil {
		return nil, err
	}
	me, err := callerOf(ctx)
	if err != nil || me.userId != memql.BareShortId(userId) {
		return nil, refuse("API keys belong to people. Sign in as yourself and try again.")
	}
	keyId := memql.BareShortId(asString(args["keyId"]))
	// REFUSED rather than read as `owner`. The modes mean different things and
	// a typo has to be told about.
	mode := strings.TrimSpace(asString(args["mode"]))
	if mode != worker.SharingModeOwner && mode != worker.SharingModePeople {
		return nil, refuse("Choose specific people and groups, or keep this key to yourself. A key cannot be lent to everyone.")
	}

	release, err := i.lockOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("setSharing", keyId, err)
	}
	defer release()
	mine, err := i.store.ActiveKeysOfOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("setSharing", keyId, err)
	}
	key, ok := find(mine, keyId)
	if !ok {
		// Not the caller's own: a cluster owner included.
		return nil, errNoSuchKey()
	}
	before := key.Sharing

	after := worker.Sharing{Mode: mode, Paused: before.Paused}
	if value, given := args["paused"]; given && value != nil {
		paused, valid := value.(bool)
		if !valid {
			return nil, refuse("Choose pause or resume for this key's lending.")
		}
		after.Paused = paused
	}
	if mode == worker.SharingModeOwner {
		// A share that was taken back leaves no names and no pause behind.
		after.Paused = false
	} else {
		// The owner is dropped rather than refused: their own key is theirs
		// already, and a list naming them says nothing about anybody else.
		after.UserIds = shareIds(args["userIds"], me.userId, key.OwnerUserId)
		after.GroupIds = shareIds(args["groupIds"])
		if total := len(after.UserIds) + len(after.GroupIds); total > memql.ShareSubjectsMax {
			return nil, refuse("A key can be lent to at most %d people and groups at once, and this names %d. A group is the way to lend it to more.", memql.ShareSubjectsMax, total)
		}
		// ONLY NEW SUBJECTS ARE CHECKED. One already on the list is the
		// owner's earlier decision; it is named even when it is no longer
		// offered, and the owner removes it by saving without it, not by being
		// refused every save until they do.
		var newUsers, newGroups []string
		for _, id := range after.UserIds {
			if !listed(before.UserIds, id) {
				newUsers = append(newUsers, id)
			}
		}
		for _, id := range after.GroupIds {
			if !listed(before.GroupIds, id) {
				newGroups = append(newGroups, id)
			}
		}
		if len(newUsers)+len(newGroups) > 0 {
			directory, err := i.directory(ctx)
			if err != nil {
				return nil, i.failed("setSharing", key.Id, err)
			}
			for _, id := range newUsers {
				if !directory.OffersPerson(id) {
					return nil, unknownSubject()
				}
			}
			for _, id := range newGroups {
				if !directory.OffersGroup(id) {
					return nil, unknownSubject()
				}
			}
		}
		after.PausedUserIds = shareIds(args["pausedUserIds"])
		after.PausedGroupIds = shareIds(args["pausedGroupIds"])
		for _, pair := range []struct{ paused, subjects []string }{
			{after.PausedUserIds, after.UserIds}, {after.PausedGroupIds, after.GroupIds},
		} {
			for _, subject := range pair.paused {
				if !listed(pair.subjects, subject) {
					return nil, refuse("Only people and groups on this key's list can be paused.")
				}
			}
		}
	}

	if err := i.store.SetSharing(ctx, me.userId, key.Id, after); err != nil {
		return nil, i.failed("setSharing", key.Id, err)
	}
	if action := lendingAction(before, after); action != "" {
		// Ids and counts, never names: who a key is lent to is in the row for
		// whoever may read it, and the log is read by more people than that.
		i.audit(ctx, me, action, key.Id, map[string]any{
			"vendor": key.Vendor, "ownerUserId": me.userId, "mode": after.Mode, "paused": after.Paused,
			"userIds": nonNil(after.UserIds), "groupIds": nonNil(after.GroupIds),
			"pausedUserIds": nonNil(after.PausedUserIds), "pausedGroupIds": nonNil(after.PausedGroupIds),
			"people": len(after.UserIds), "groups": len(after.GroupIds),
		})
	}
	return i.cardReply(ctx, me, key.Id, "")
}

// lendingAction names what a change of lending was, or "" when nothing about
// who may spend the key changed.
func lendingAction(before, after worker.Sharing) string {
	lends := func(s worker.Sharing) bool {
		return s.Mode == worker.SharingModePeople && len(s.UserIds)+len(s.GroupIds) > 0
	}
	switch {
	case lends(before) && !lends(after):
		return auditLendingStopped
	case !lends(after):
		return ""
	case !lends(before):
		return auditLent
	}
	// Lent before and after. A pause is the narrower change and is named
	// first; a change of audience with the pauses as they were is a lend.
	pausedBefore := pausedCount(before)
	pausedAfter := pausedCount(after)
	switch {
	case pausedAfter > pausedBefore:
		return auditLendingPaused
	case pausedAfter < pausedBefore:
		return auditLendingResumed
	case sameIds(before.UserIds, after.UserIds) && sameIds(before.GroupIds, after.GroupIds):
		return ""
	}
	return auditLent
}

// pausedCount orders two sharings by how much of the audience is paused. A
// whole pause outweighs any number of single ones.
func pausedCount(s worker.Sharing) int {
	n := len(s.PausedUserIds) + len(s.PausedGroupIds)
	if s.Paused {
		n += 2 * (memql.ShareSubjectsMax + 1)
	}
	return n
}

// handleShareDirectory answers who one of the caller's keys can be lent to and
// who it is lent to now.
func (i *Integration) handleShareDirectory(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	me, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	key, found, err := i.store.Key(ctx, asString(args["keyId"]))
	if err != nil {
		return nil, i.failed("shareDirectory", memql.BareShortId(asString(args["keyId"])), err)
	}
	if !found || !key.Active() || key.OwnerUserId != me.userId {
		return nil, errNoSuchKey()
	}
	directory, err := i.directory(ctx)
	if err != nil {
		return nil, i.failed("shareDirectory", key.Id, err)
	}

	people := make([]any, 0, len(directory.People))
	for _, p := range directory.People {
		people = append(people, map[string]any{"id": p.Id, "name": p.Name, "detail": p.Detail})
	}
	groups := make([]any, 0, len(directory.Groups))
	for _, g := range directory.Groups {
		groups = append(groups, map[string]any{"id": g.Id, "name": g.Name, "members": g.Members})
	}
	// THE LIST ALREADY ON THE KEY, named whether or not it is still offered,
	// so the owner can see who has it and take off somebody who has since left
	// their groups. `known` false is a subject no row names any more: shown
	// and removable, never guessed at.
	currentPeople := make([]any, 0, len(key.Sharing.UserIds))
	for _, id := range key.Sharing.UserIds {
		bare := memql.BareShortId(id)
		entry, known := directory.Person(bare)
		currentPeople = append(currentPeople, map[string]any{
			"id": bare, "name": entry.Name, "known": known, "inDirectory": directory.OffersPerson(bare),
			"paused": listed(key.Sharing.PausedUserIds, bare),
		})
	}
	currentGroups := make([]any, 0, len(key.Sharing.GroupIds))
	for _, id := range key.Sharing.GroupIds {
		bare := memql.BareShortId(id)
		entry, known := directory.Group(bare)
		currentGroups = append(currentGroups, map[string]any{
			"id": bare, "name": entry.Name, "known": known, "inDirectory": directory.OffersGroup(bare),
			"paused": listed(key.Sharing.PausedGroupIds, bare),
		})
	}
	return i.node("shareDirectory:"+key.Id, map[string]any{
		"keyId":         key.Id,
		"everyone":      directory.Everyone,
		"people":        people,
		"groups":        groups,
		"currentPeople": currentPeople,
		"currentGroups": currentGroups,
		"mode":          key.Sharing.Mode,
		"paused":        key.Sharing.Paused,
		"max":           memql.ShareSubjectsMax,
	})
}

// shareIds reads a list of subject ids as bare ids, once each, without the
// ids in skip.
func shareIds(v any, skip ...string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range skip {
		if bare := memql.BareShortId(strings.TrimSpace(s)); bare != "" {
			seen[bare] = true
		}
	}
	var raw []string
	switch list := v.(type) {
	case []string:
		raw = list
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	for _, s := range raw {
		bare := memql.BareShortId(strings.TrimSpace(s))
		if bare == "" || seen[bare] {
			continue
		}
		seen[bare] = true
		out = append(out, bare)
	}
	return out
}

func listed(list []string, id string) bool {
	for _, have := range list {
		if worker.SameSubjectId(have, id) {
			return true
		}
	}
	return false
}

func sameIds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, id := range a {
		if !listed(b, id) {
			return false
		}
	}
	return true
}
