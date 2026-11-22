package vendorkeys

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/component/worker"
)

// resolver.go -- which of a person's API keys can pay for a call (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K6 to K9, K11, K16, safety rule S9, mesh rule M1). It is the
// memql.VendorKeys the provider registry asks, installed on every node.
//
// # The rule, in the order it is applied
//
// WHO IS ASKING. A call with no acting person, a synthetic actor, or a call
// made for a visitor under a person's borrowed authority uses no key: the
// answer is empty and no row is read. The registry asks the same question
// before it calls this (keysServe, component/memql); it is asked again here
// because this is the seam that touches rows, and it must not depend on its
// caller having asked.
//
// WHICH KEYS ARE CANDIDATES (K7, K8). Of the vendor's active keys:
//
//   - the person's own DEFAULT key at the vendor, first. Their other keys at
//     the vendor are not candidates: a route that pins one reaches it through
//     Usable, and nothing else spends it;
//   - then every key of another owner whose sharing admits the person, the key
//     added earliest first, ties by id so every replica agrees. Admission is a
//     machine's (worker.Sharing.Admits): named, or through an active group, and
//     not while the grant or the whole share is paused.
//
// When nobody chose the route (Auto), only keys whose owner switched Auto on
// are candidates.
//
// WHICH CANDIDATES ARE PASSED OVER, AND WHY (K9). A key the vendor refused; a
// key whose account is out of credit, until its hold runs out; a key whose
// owner is no longer an active user; a key lent to the person and paused for
// them. Each is reported with its reason, which goes on the decision record
// every signed-in person can read, so a reason never names a key: no label, no
// fingerprint.
//
// A key the person has no tie to is neither offered nor reported. It is not
// theirs to hear about.
//
// # Nothing is remembered
//
// Every answer is computed from rows read fresh at the time of the call
// (store.go). A replica that never saw a key before decides the same as the
// one its owner is connected to, a second after the owner changed it.

// creditHold is how long a key whose account is out of credit is passed over
// before the next real call may try it again (K11).
const creditHold = time.Hour

// notUsableReason is the one sentence Usable gives for every key a call cannot
// use. An unknown id, a removed key, another vendor's key and a key this
// person may not use are told apart by nothing a caller can see (S9).
const notUsableReason = "that API key is not one this call can use"

// Resolver answers the provider registry's questions about keys.
type Resolver struct {
	store  *Store
	groups worker.GroupResolver
}

var _ memql.VendorKeys = (*Resolver)(nil)

// NewResolver builds the resolver. A nil groups is the membership source every
// node installs (worker.InstalledGroups).
func NewResolver(store *Store, groups worker.GroupResolver) *Resolver {
	return &Resolver{store: store, groups: groups}
}

// asks reports whether a key may pay for this query at all, before any row is
// read: never for the cluster's own work, and never for a visitor.
func asks(ctx context.Context, q memql.VendorAccountQuery) (person string, ok bool) {
	person = memql.BareShortId(strings.TrimSpace(q.ActingUserId))
	if person == "" || q.ForVisitor || q.Vendor == "" || auth.ActsForNoPerson(ctx, q.ActingUserId) || auth.NamesNoPerson(person) {
		return "", false
	}
	return person, true
}

// KeysFor implements memql.VendorKeys.
func (r *Resolver) KeysFor(ctx context.Context, q memql.VendorAccountQuery) ([]memql.VendorAccount, []memql.VendorAccountPassedOver, error) {
	person, ok := asks(ctx, q)
	if !ok {
		return nil, nil, nil
	}
	keys, err := r.store.ActiveKeysAtVendor(ctx, q.Vendor)
	if err != nil {
		return nil, nil, err
	}
	judge := r.newJudge(ctx, person, q)

	// Candidates in the order they are tried. ActiveKeysAtVendor returns keys
	// in the order they were added, so the lent ones are already in K8's order.
	var own *Key
	var lent []Key
	var passedOver []memql.VendorAccountPassedOver
	for i := range keys {
		key := keys[i]
		switch judge.tie(key) {
		case tieOwnDefault:
			if own == nil && judge.countsForRoute(key) {
				own = &keys[i]
			}
		case tieLent:
			if judge.countsForRoute(key) {
				lent = append(lent, key)
			}
		case tieLentPaused:
			if judge.countsForRoute(key) {
				passedOver = append(passedOver, passedOverKey(key, "an API key's owner has paused lending it"))
			}
		}
	}
	candidates := make([]Key, 0, len(lent)+1)
	if own != nil {
		candidates = append(candidates, *own)
	}
	candidates = append(candidates, lent...)
	if len(candidates) == 0 {
		return nil, passedOver, nil
	}

	ids := make([]string, 0, len(candidates))
	for _, key := range candidates {
		ids = append(ids, key.Id)
	}
	states, err := r.store.States(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	var usable []memql.VendorAccount
	for _, key := range candidates {
		reason, err := judge.unfit(key, states[key.Id])
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			passedOver = append(passedOver, passedOverKey(key, reason))
			continue
		}
		usable = append(usable, accountOf(key))
	}
	return usable, passedOver, nil
}

// Usable implements memql.VendorKeys: the same judgment for one key, at the
// time of the call. It also admits what KeysFor never offers, a person's own
// key that is not their default, because a route may pin one (K7).
func (r *Resolver) Usable(ctx context.Context, q memql.VendorAccountQuery, keyId string) (memql.VendorAccount, string, bool) {
	person, ok := asks(ctx, q)
	keyId = memql.BareShortId(keyId)
	if !ok || keyId == "" {
		return memql.VendorAccount{}, notUsableReason, false
	}
	key, found, err := r.store.Key(ctx, keyId)
	if err != nil || !found || !key.Active() || key.Vendor != q.Vendor {
		return memql.VendorAccount{}, notUsableReason, false
	}
	judge := r.newJudge(ctx, person, q)
	switch judge.tie(key) {
	case tieOwnDefault, tieOwnOther, tieLent:
	default:
		return memql.VendorAccount{}, notUsableReason, false
	}
	if !judge.countsForRoute(key) {
		return memql.VendorAccount{}, notUsableReason, false
	}
	states, err := r.store.States(ctx, []string{key.Id})
	if err != nil {
		return memql.VendorAccount{}, notUsableReason, false
	}
	if reason, err := judge.unfit(key, states[key.Id]); err != nil || reason != "" {
		return memql.VendorAccount{}, notUsableReason, false
	}
	return accountOf(key), "", true
}

// Secret implements memql.VendorKeys: a key's plain value, for the one request
// it pays for. A removed key is never unsealed again (K16).
//
// The refusals are account failures of the kind that was never sent to the
// vendor, so the router moves to the next account for the same model rather
// than to the next model (K10).
func (r *Resolver) Secret(ctx context.Context, a memql.VendorAccount) (string, error) {
	if a.Kind != memql.VendorAccountKey || memql.BareShortId(a.KeyId) == "" {
		return "", unusable(a.Vendor, "this call names no API key")
	}
	value, vendor, status, found, err := r.store.sealed(ctx, a.KeyId)
	switch {
	case err != nil:
		return "", unusable(a.Vendor, "the API key could not be read")
	case !found || status != statusActive:
		return "", unusable(a.Vendor, "the API key was removed")
	case a.Vendor != "" && vendor != a.Vendor:
		return "", unusable(a.Vendor, "the API key is for another vendor")
	}
	plain, err := secret.Decrypt(value.reveal())
	if err != nil {
		// The cause can be the master key's absence, which an operator fixes,
		// and it never contains the value. Its words stay in the log.
		r.store.logger.Error("vendorkeys: an API key could not be unsealed", "keyId", memql.BareShortId(a.KeyId), "error", err)
		return "", unusable(a.Vendor, "the API key could not be unsealed on this node")
	}
	return plain, nil
}

func unusable(vendor, reason string) error {
	return &memql.VendorAccountError{Vendor: vendor, Kind: "unavailable", Reason: reason}
}

func accountOf(key Key) memql.VendorAccount {
	return memql.VendorAccount{Kind: memql.VendorAccountKey, Vendor: key.Vendor, KeyId: key.Id, OwnerUserId: key.OwnerUserId}
}

func passedOverKey(key Key, reason string) memql.VendorAccountPassedOver {
	return memql.VendorAccountPassedOver{Account: accountOf(key), Reason: reason}
}

// A key's tie to the person a call is for.
type keyTie int

const (
	// tieNone: somebody else's key, not lent to this person.
	tieNone keyTie = iota
	// tieOwnDefault: the person's own default key at the vendor.
	tieOwnDefault
	// tieOwnOther: the person's own key, not their default.
	tieOwnOther
	// tieLent: another owner's key whose sharing admits the person.
	tieLent
	// tieLentPaused: another owner's key that names the person, or a group of
	// theirs, and is paused for them.
	tieLentPaused
)

// judge holds what one answer needs to read at most once: the person's groups
// and whether each key's owner is still an active user.
type judge struct {
	ctx    context.Context
	r      *Resolver
	person *worker.Person
	userId string
	chosen bool
	now    time.Time
	active map[string]bool
}

func (r *Resolver) newJudge(ctx context.Context, person string, q memql.VendorAccountQuery) *judge {
	return &judge{
		ctx: ctx, r: r, userId: person, chosen: q.Chosen, now: r.store.now(),
		person: worker.NewPerson(ctx, person, r.groups),
		active: map[string]bool{},
	}
}

func (j *judge) tie(key Key) keyTie {
	if worker.SameSubjectId(key.OwnerUserId, j.userId) {
		if key.IsDefault {
			return tieOwnDefault
		}
		return tieOwnOther
	}
	if key.Sharing.Admits(j.person) {
		return tieLent
	}
	if namedButPaused(key.Sharing, j.person) {
		return tieLentPaused
	}
	return tieNone
}

// countsForRoute is K8's last line: a call nobody chose a route for counts
// only keys whose owner switched Auto on.
func (j *judge) countsForRoute(key Key) bool { return j.chosen || key.AutoUse }

// unfit is why a candidate cannot pay right now, or "" when it can.
func (j *judge) unfit(key Key, state State) (string, error) {
	vendor := vendorName(key.Vendor)
	switch state.State {
	case StateRefused:
		return fmt.Sprintf("an API key was refused by %s", vendor), nil
	case StateCredit:
		if state.HeldUntil.After(j.now) {
			return fmt.Sprintf("an API key's account is out of credit at %s", vendor), nil
		}
	}
	active, known := j.active[key.OwnerUserId]
	if !known {
		var err error
		if active, err = j.r.store.UserIsActive(j.ctx, key.OwnerUserId); err != nil {
			return "", err
		}
		j.active[key.OwnerUserId] = active
	}
	if !active {
		return "an API key's owner is no longer an active user", nil
	}
	return "", nil
}

// namedButPaused reports whether a share names this person, directly or
// through one of their groups, and admits them through none of those grants.
// It is what separates "its owner paused lending it to you" from "it was never
// lent to you", and only the first is the person's to hear about.
func namedButPaused(s worker.Sharing, p *worker.Person) bool {
	if s.Mode != worker.SharingModePeople || p == nil || p.UserId == "" || s.Admits(p) {
		return false
	}
	for _, id := range s.UserIds {
		if worker.SameSubjectId(id, p.UserId) {
			return true
		}
	}
	if len(s.GroupIds) == 0 {
		return false
	}
	for _, have := range p.Groups() {
		for _, want := range s.GroupIds {
			if worker.SameSubjectId(have, want) {
				return true
			}
		}
	}
	return false
}

func vendorName(vendor string) string {
	switch vendor {
	case memql.VendorAnthropic:
		return "Anthropic"
	case memql.VendorOpenAI:
		return "OpenAI"
	}
	return vendor
}
