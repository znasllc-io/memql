package worker

import (
	"context"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

// Model sharing is the enrolled owner's decision, stored on registration.sharing.
// Fleet is the single control: private by default, or shared with named people
// and active group members. A worker's labels and capability advertisement
// cannot grant access. Machine model allowlists and execution permissions remain
// independent; sharing authorizes inference, never files, commands or apps.
// Every catalog, routing path and receiving replica uses these predicates.
// A synthetic system actor can use only a legacy cluster-wide share; an
// automation acting for a person has that person's access.

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

// Sharing is the owner's decision, as stored on the registration row.
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

// ServesTheCluster admits system work only to a legacy cluster-wide share.
func ServesTheCluster(ownerMode string) bool {
	return strings.TrimSpace(ownerMode) == SharingModeCluster
}

// SharingRefusal explains the owner's sharing boundary without naming a
// private machine. Fleet is the only place the owner needs to configure it.
func SharingRefusal(ownerMode string) string {
	if ServesTheCluster(ownerMode) {
		return ""
	}
	return "Its owner has not shared this machine with the cluster. The owner can choose who may use its models in Fleet."
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

// Admits reports whether the owner's saved audience includes this person.
//
// NO ACTING PERSON IS NEVER ADMITTED, in any mode. An empty id is system work,
// and system work asks ServesTheCluster instead (design G3) -- so a caller that
// failed to resolve who it is acting for cannot reach a machine through the
// person path by accident.
func (s Sharing) Admits(p *Person) bool {
	if p == nil || p.UserId == "" {
		return false
	}
	if auth.IsSystemActorId(p.UserId) {
		// A SYNTHETIC ACTOR IS THE CLUSTER'S OWN WORK, never a person on a
		// list. Automations run as `system:automation:<name>` and maintenance
		// as `system:maintenance:<name>`, so their calls carry an id -- and a
		// comparison that matched one to a listed person by the text after its
		// last colon would lend a machine to an automation because its name
		// happened to equal somebody's id. Only a machine lent to everyone
		// serves the cluster's own work (design G3), by construction. An
		// automation under a person's BORROWED authority never reaches this
		// branch: it carries that person's id and is admitted as them
		// (memql#5662).
		return s.Mode == SharingModeCluster
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

// ServesPerson is the shared admission predicate for catalogs, routing and the
// replica-hop receiver. No separate machine configuration is needed.
func ServesPerson(s Sharing, p *Person) bool { return s.Admits(p) }

// PersonRefusal is counted, never named, for a foreign machine. Keep the
// classifier in component/memql in step; sharing_parity_test enforces this.
func PersonRefusal(s Sharing, p *Person) string {
	if ServesPerson(s, p) {
		return ""
	}
	if s.Mode == SharingModePeople {
		return "Its owner has shared it with specific people, and not with you."
	}
	return SharingRefusal(s.Mode)
}

// SystemRefusal keeps a people share closed to the cluster's own work.
func SystemRefusal(s Sharing) string {
	if ServesTheCluster(s.Mode) {
		return ""
	}
	if s.Mode == SharingModePeople {
		return "Its owner has shared it with specific people, not with the cluster's own work."
	}
	return SharingRefusal(s.Mode)
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

// bareSubjectId is component/memql.BareShortId's rule, through the same
// parser: the short id after a `v<N>:domain:entity` concept prefix, and the
// whole value when there is no such prefix. NOT the text after the last colon
// -- that rule read `system:automation:ana` as `ana` and `a:b` as `c:b`, and
// the two readers of one stored share must collapse ids identically.
func bareSubjectId(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	_, short, err := id.ParseNodeId(v)
	if err != nil || short == "" {
		return v
	}
	return short
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
