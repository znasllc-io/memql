package vendorkeys

import (
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/worker"
)

// cards.go -- what a caller is shown of a key (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K5).
//
// TWO CARDS, AND THE DIFFERENCE IS THE AUTHORIZATION. A key's owner, and a
// cluster owner reading every key's facts, get the FULL card. A person a key is
// lent to gets the SHORT card: its name, its vendor, whose it is and whether it
// works, which is what they need to recognise what is paying for their calls
// and nothing about how its owner has set it up.
//
// NO CARD CARRIES THE VALUE. Key has no field for it (store.go), so there is
// nothing here that could.

// The relation of a card's key to the person it is shown to.
const (
	relationMine  = "mine"
	relationLent  = "lent"
	relationOther = "other"
)

// StateUnchecked is the state of a key nothing has been learned about yet: one
// that may not list models (K12), until the first real call it pays for.
const StateUnchecked = "unchecked"

func relationOf(tie keyTie) string {
	switch tie {
	case tieOwnDefault, tieOwnOther:
		return relationMine
	case tieLent, tieLentPaused:
		return relationLent
	}
	return relationOther
}

// stateOf is what the engine last recorded about a key, with the vendor's words
// behind a refusal or a hold. A key with no record reads as unchecked.
func stateOf(key Key, states map[string]State) (word, detail string, state State) {
	state, known := states[key.Id]
	if !known || state.State == "" {
		return StateUnchecked, "", state
	}
	switch state.State {
	case StateRefused, StateCredit:
		return state.State, state.Detail, state
	}
	return state.State, "", state
}

// fullCard is the owner's card, and a cluster owner's. `heldUntil` is set only
// while a credit hold is still running: after it the next real call tries the
// key, and the card should not promise a wait that is over.
func fullCard(key Key, states map[string]State, names map[string]string, relation string, now time.Time) map[string]any {
	word, detail, state := stateOf(key, states)
	card := map[string]any{
		"id":          key.Id,
		"vendor":      key.Vendor,
		"label":       key.Label,
		"fingerprint": key.Fingerprint,
		"isDefault":   key.IsDefault,
		"autoUse":     key.AutoUse,
		"checkedBy":   key.CheckedBy,
		"status":      key.Status,
		"state":       word,
		"detail":      detail,
		"addedAt":     timeOrEmpty(key.AddedAt),
		"checkedAt":   timeOrEmpty(state.CheckedAt),
		"lastUsedAt":  timeOrEmpty(state.LastUsedAt),
		"heldUntil":   "",
		"lentTo":      len(key.Sharing.UserIds) + len(key.Sharing.GroupIds),
		"ownerUserId": key.OwnerUserId,
		"ownerName":   names[key.OwnerUserId],
		"relation":    relation,
	}
	if word == StateCredit && state.HeldUntil.After(now) {
		card["heldUntil"] = timeOrEmpty(state.HeldUntil)
	}
	// WHO IT IS LENT TO is on the owner's own card and no other. It is their
	// setting, and the page that edits it starts from it. A cluster owner
	// reading every key's facts gets the count above and not the names (K5):
	// they may stop a key, not see who its owner trusts with it.
	if relation == relationMine {
		card["sharing"] = map[string]any{
			"mode":           sharingModeOf(key),
			"paused":         key.Sharing.Paused,
			"userIds":        bareIds(key.Sharing.UserIds),
			"groupIds":       bareIds(key.Sharing.GroupIds),
			"pausedUserIds":  bareIds(key.Sharing.PausedUserIds),
			"pausedGroupIds": bareIds(key.Sharing.PausedGroupIds),
		}
	}
	return card
}

// sharingModeOf is a key's lending mode in the two words a key has: its
// owner's alone, or named people and groups.
func sharingModeOf(key Key) string {
	if key.Sharing.Mode == worker.SharingModePeople {
		return worker.SharingModePeople
	}
	return worker.SharingModeOwner
}

// bareIds is a list of ids as a client compares them, never nil.
func bareIds(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if bare := memql.BareShortId(id); bare != "" {
			out = append(out, bare)
		}
	}
	return out
}

// lentCard is what a person a key is lent to sees of it. `paused` says its
// owner has paused lending it to them, which is why it is not paying.
func lentCard(key Key, states map[string]State, names map[string]string, paused bool) map[string]any {
	word, _, _ := stateOf(key, states)
	return map[string]any{
		"id":          key.Id,
		"vendor":      key.Vendor,
		"label":       key.Label,
		"ownerUserId": key.OwnerUserId,
		"ownerName":   names[key.OwnerUserId],
		"state":       word,
		"paused":      paused,
		"relation":    relationLent,
	}
}

func timeOrEmpty(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}
