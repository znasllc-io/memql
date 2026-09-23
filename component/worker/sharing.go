package worker

import (
	"context"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
)

// SHARING: two consents, and neither one alone (epic memql#5146, design D6).
//
// ===========================================================================
// THE PROBLEM THIS SOLVES
// ===========================================================================
// A fleet machine serves only its owner's calls. A business with one Mac Studio
// in the office has no way to make it serve the team, so "local by default" is
// per PERSON rather than per COMPANY -- and the cost principle this platform is
// built on applies to an individual instead of to the business whose logic it is
// meant to hand back to them.
//
// ===========================================================================
// WHY TWO CONSENTS AND NOT ONE FLAG
// ===========================================================================
// The owner's consent is a decision about their property. The cockpit's is a
// decision about the machine's SITUATION: it may be a laptop on a train, a
// build box under somebody's desk, a machine in a room where running strangers'
// prompts is not allowed. Those are different questions and different people
// answer them, so a single flag would let either one speak for the other.
//
// It is the `allowed` and `signedIn` shape the local-app door already uses, and
// the reasoning is the same: the machine's own policy.yaml is the only thing
// that knows what the machine is for.
//
// ===========================================================================
// THE REFUSAL NAMES WHICH HALF IS MISSING
// ===========================================================================
// The two repairs are in different places -- the owner's is one act on the
// Fleet page, the cockpit's is a line in that machine's policy.yaml -- so a
// single "not shared" sentence sends half the operators to the wrong machine.
// SharingRefusal is what stops that being a UI decision.
//
// ===========================================================================
// LENT TO PEOPLE, NOT ONLY TO EVERYONE (epic memql#5344)
// ===========================================================================
// The owner's half has a third answer: `people`, with the users and groups the
// machine is lent to. It is the same consent narrowed, and it lives on the same
// row for the same reason -- it is the OWNER'S decision about their property,
// which is why it is not a v1:rbac:grant (those are written under admin
// governance, and a person lending their own laptop is not an admin act).
//
// The cockpit's half keeps its two values. `inference.serve: cluster` now
// reads "this machine may serve people other than its owner"; WHICH people is
// the owner's half. A people share is never a cluster share: the cluster's own
// work -- automations, maintenance, anything with no acting person -- reaches
// only machines lent to everyone (design G3), because a list of names is
// consent for those people and automations are nobody on the list.

// SharingMode values on registration.sharing.mode.
const (
	// SharingModeOwner is the default and the absent case: this machine serves
	// its owner's calls and nobody else's.
	SharingModeOwner = "owner"
	// SharingModeCluster is the owner offering the machine to everyone.
	SharingModeCluster = "cluster"
	// SharingModePeople is the owner lending the machine to the users and the
	// ACTIVE members of the ACTIVE groups named beside it (epic memql#5344).
	SharingModePeople = "people"
)

// Sharing is the owner's half, as stored on the registration row.
//
// UserIds and GroupIds mean something ONLY under SharingModePeople, and
// SharingFromRow leaves them empty for every other mode: a list stored beside
// `owner` or `cluster` is residue from an earlier share, and honouring it would
// lend a machine its owner had taken back.
type Sharing struct {
	Mode     string
	UserIds  []string
	GroupIds []string
	SharedAt string
	SharedBy string
}

// SharingFromRow reads registration.sharing.
//
// A MISSING KEY IS `owner`, and here the safe reading and the honest reading
// coincide: a row written before the field existed carries no consent, and no
// consent is not consent.
func SharingFromRow(v any) Sharing {
	row, ok := v.(map[string]any)
	if !ok || len(row) == 0 {
		return Sharing{Mode: SharingModeOwner}
	}
	mode, _ := row["mode"].(string)
	mode = strings.TrimSpace(mode)
	if mode != SharingModeCluster && mode != SharingModePeople {
		// Anything that is not exactly `cluster` or `people` is `owner`. A
		// typo, a value from a future engine, a half-written row: all of them
		// are read as NOT shared, because the failure direction here is
		// somebody's laptop running a stranger's prompt.
		mode = SharingModeOwner
	}
	sharedAt, _ := row["sharedAt"].(string)
	sharedBy, _ := row["sharedBy"].(string)
	out := Sharing{Mode: mode, SharedAt: sharedAt, SharedBy: sharedBy}
	if mode == SharingModePeople {
		out.UserIds = subjectIds(row["userIds"])
		out.GroupIds = subjectIds(row["groupIds"])
		if len(out.UserIds) == 0 && len(out.GroupIds) == 0 {
			// LENT TO NOBODY IS NOT LENT. fleetSetSharing refuses to write this,
			// so it is a hand-edited or half-written row -- and reading it as
			// `owner` is both the safe answer and the honest one.
			out.Mode = SharingModeOwner
		}
	}
	return out
}

// Row renders the owner's consent for storage.
//
// BOTH LISTS ARE ALWAYS PRESENT, and empty outside `people` (design G10), so a
// stored block says what it lends in every mode rather than leaving a reader to
// infer an absent key.
func (s Sharing) Row() map[string]any {
	mode := s.Mode
	if mode != SharingModeCluster && mode != SharingModePeople {
		mode = SharingModeOwner
	}
	userIds, groupIds := []any{}, []any{}
	if mode == SharingModePeople {
		for _, id := range s.UserIds {
			userIds = append(userIds, id)
		}
		for _, id := range s.GroupIds {
			groupIds = append(groupIds, id)
		}
	}
	return map[string]any{
		"mode":     mode,
		"userIds":  userIds,
		"groupIds": groupIds,
		"sharedAt": s.SharedAt,
		"sharedBy": s.SharedBy,
	}
}

// ServesTheCluster reports whether BOTH consents say cluster.
//
// `cockpitServe` is capabilityDescriptor.inferenceServe, and an EMPTY value is
// `owner`: a cockpit that predates the field has said nothing, and silence is
// not agreement.
func ServesTheCluster(ownerMode, cockpitServe string) bool {
	return strings.TrimSpace(ownerMode) == SharingModeCluster &&
		strings.TrimSpace(cockpitServe) == InferenceServeCluster
}

// SharingRefusal names WHICH consent is missing, in the words of the repair.
//
// Empty when the machine does serve the cluster. The two sentences are
// different because the two fixes are in different places and are performed by
// different people; a single "not shared" would send half the operators to the
// wrong machine, and the one who owns the laptop would go looking on a web page
// for a setting that lives in a file on their own disk.
func SharingRefusal(ownerMode, cockpitServe string) string {
	ownerSaid := strings.TrimSpace(ownerMode) == SharingModeCluster
	cockpitSaid := strings.TrimSpace(cockpitServe) == InferenceServeCluster
	switch {
	case ownerSaid && cockpitSaid:
		return ""
	case !ownerSaid && !cockpitSaid:
		return "Neither consent is given: the owner has not shared this machine with the cluster, and its cockpit's policy.yaml does not set inference.serve to cluster. Both are needed."
	case !ownerSaid:
		return "The machine's cockpit is willing to serve the cluster, but its owner has not shared it. The owner turns this on from the machine's page in Fleet."
	default:
		return "The owner has shared this machine, but its cockpit is not willing to serve the cluster. Set inference.serve to cluster in that machine's policy.yaml -- it is a decision about where the machine is, and only the machine can make it."
	}
}

// ---------------------------------------------------------------------------
// Who a shared machine is being asked about (epic memql#5344, G6 and G7)
// ---------------------------------------------------------------------------

// GroupResolver answers a person's ACTIVE group ids: active memberships in
// active groups, the answer the account scope and the grant resolver already
// read. Injected so a test can say who is in which group; production uses
// InstalledGroups.
type GroupResolver func(ctx context.Context, userId string) []string

// InstalledGroups resolves through the membership source every node installs
// at engine start (component/memql.InstallGrantResolution).
//
// A NIL SOURCE IS NO GROUPS, and that is the narrowing direction: listed people
// and `cluster` shares still work, and a group share admits nobody until the
// source is there. Admitting on a membership nobody could read would be the
// widening direction, and it is the one this function exists not to take.
func InstalledGroups(ctx context.Context, userId string) []string {
	if ms := auth.InstalledMembershipSource(); ms != nil {
		return ms.ActiveGroupIdsForUser(ctx, userId)
	}
	return nil
}

// Person is the acting person a share is asked about. Their groups are read AT
// MOST ONCE, and only when a share that names a group actually needs them: a
// membership read per candidate per call would be a cost paid by every fleet,
// whether or not anybody in it had ever shared with a group.
//
// Use it by pointer. The sync.Once is what makes "at most once" true, and a
// copied Person would resolve again.
type Person struct {
	UserId string

	resolve func() []string
	once    sync.Once
	groups  []string
}

// NewPerson builds the person for one call. A nil resolver is InstalledGroups.
func NewPerson(ctx context.Context, userId string, resolver GroupResolver) *Person {
	userId = strings.TrimSpace(userId)
	if resolver == nil {
		resolver = InstalledGroups
	}
	return &Person{
		UserId: userId,
		resolve: func() []string {
			if userId == "" {
				return nil
			}
			return resolver(ctx, userId)
		},
	}
}

// Groups returns the person's active groups, resolving them on first use.
func (p *Person) Groups() []string {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		if p.resolve != nil {
			p.groups = p.resolve()
		}
	})
	return p.groups
}

// Admits reports whether the OWNER'S half lets this person use the machine.
// It is half of the question; ServesPerson is the whole of it.
//
// NO ACTING PERSON IS NEVER ADMITTED, in any mode. An empty id is system work,
// and system work asks ServesTheCluster instead (design G3) -- so a caller that
// failed to resolve who it is acting for cannot reach a machine through the
// person path by accident.
func (s Sharing) Admits(p *Person) bool {
	if p == nil || p.UserId == "" {
		return false
	}
	switch s.Mode {
	case SharingModeCluster:
		return true
	case SharingModePeople:
		for _, id := range s.UserIds {
			if SameSubjectId(id, p.UserId) {
				return true
			}
		}
		if len(s.GroupIds) == 0 {
			// The lazy half: a share naming no group never reads a membership.
			return false
		}
		for _, have := range p.Groups() {
			for _, want := range s.GroupIds {
				if SameSubjectId(have, want) {
					return true
				}
			}
		}
	}
	return false
}

// ServesPerson reports whether BOTH consents let this person's call run on the
// machine: the cockpit said it may serve people other than its owner, and the
// owner lent it to this person. The ONE predicate every person-path reader
// asks -- the catalog, the shared plan and the replica-hop receiver -- so that
// "is this machine lent to you" has one answer in the tree (design G7).
func ServesPerson(s Sharing, cockpitServe string, p *Person) bool {
	return strings.TrimSpace(cockpitServe) == InferenceServeCluster && s.Admits(p)
}

// PersonRefusal names why a machine does not serve this person, or "" when it
// does. The owner-only and cluster cases keep SharingRefusal's two-consent
// sentences; a people share gets its own, because "not shared" is the wrong
// repair for somebody who is not on a list that exists.
//
// These sentences reach a routing plan's rejected map, where a machine that is
// not the caller's is COUNTED and never named (memql#5327, D12) --
// component/memql.IsForeignShareRefusal must recognise every one of them, and
// a parity test in integrations/agent/worker holds the two together.
func PersonRefusal(s Sharing, cockpitServe string, p *Person) string {
	if ServesPerson(s, cockpitServe, p) {
		return ""
	}
	if s.Mode != SharingModePeople {
		return SharingRefusal(s.Mode, cockpitServe)
	}
	if !s.Admits(p) {
		return "Its owner has shared it with specific people, and not with you."
	}
	return "Its owner has shared it with you, but its cockpit is not willing to serve anyone but its owner. Set inference.serve to cluster in that machine's policy.yaml -- it is a decision about where the machine is, and only the machine can make it."
}

// SystemRefusal names why a machine does not serve the cluster's own work, or
// "" when it does. A people share is refused with its own sentence (design G3),
// because the two-consent sentence would tell an operator to turn on a share
// the owner has already turned on -- for somebody else.
func SystemRefusal(s Sharing, cockpitServe string) string {
	if ServesTheCluster(s.Mode, cockpitServe) {
		return ""
	}
	if s.Mode == SharingModePeople {
		return "Its owner has shared it with specific people, not with the cluster's own work."
	}
	return SharingRefusal(s.Mode, cockpitServe)
}

// SameSubjectId compares two identity ids tolerantly of the bare/canonical
// split. A share list stores what the owner's client sent, a token's subject
// may be bare, and a group id arrives canonical from the membership source --
// so a naive == refuses the one person the list names.
func SameSubjectId(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || bareSubjectId(a) == bareSubjectId(b)
}

func bareSubjectId(v string) string {
	if i := strings.LastIndex(v, ":"); i >= 0 {
		return v[i+1:]
	}
	return v
}

// subjectIds reads a stored id list: trimmed, empties dropped, and one entry
// per subject however many spellings of it the list carries (the first
// spelling is kept). Accepts the []any a JSON decode produces and a []string.
func subjectIds(v any) []string {
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
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := bareSubjectId(id)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
