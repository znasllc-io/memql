package memql

// WHO AN OWNER MAY LEND A MACHINE TO (epic memql#5344, design G2 and G9).
//
// ===========================================================================
// THE DIRECTORY IS WHAT THE OWNER ALREADY KNOWS, AND NOTHING MORE
// ===========================================================================
// A share dialog needs names to pick from, and a plain user has no read that
// lists anybody: the roster is floored at developer, groups at admin. Opening
// the whole roster to every machine owner -- client-rank people included --
// would be a new visibility bought for a dialog. So the directory is the set
// the owner can already account for:
//
//   - the ACTIVE groups they are an ACTIVE member of, and
//   - the active people in those groups,
//
// and a caller who may already read every person -- `read` on `principal`,
// which owner, developer and admin hold by seed and a grant can give or take
// away -- is offered everyone, with the email that read already shows them.
// Anybody else is never handed an email; a display name is the most the
// engine publishes about one person to another (userDisplayById).
//
// ===========================================================================
// IT ANSWERS ONLY AN OWNER, ABOUT ONE OF THEIR OWN MACHINES
// ===========================================================================
// The builtin resolves the machine through the caller's own machines first,
// so a person with no machine cannot enumerate anybody through it, and a
// machine that is not theirs answers exactly as a made-up id does.
//
// ===========================================================================
// THE WRITE IS CHECKED AGAINST THE SAME ANSWER
// ===========================================================================
// fleetSetSharing refuses a NEW subject the directory does not offer, with one
// sentence for "does not exist" and "not yours to pick" -- so the write is not
// an oracle for who is on the cluster. A subject ALREADY on the machine's list
// is not re-checked: somebody who left the owner's group is still on the list
// the owner wrote, the dialog names them, and the owner removes them by
// saving without them. Refusing every later save until they did would turn an
// edit of one name into a failure about another.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ShareDirectoryConcept names the virtual row fleetShareDirectory answers
// with. Nothing is persisted: it is a view over three concepts' rows.
const ShareDirectoryConcept = "v1:worker:shareDirectory"

// shareSubjectsMax bounds a people share across both lists (design G9). A
// share is a handful of colleagues or a group; fifty is room for every real
// case and small enough that a stored consent stays readable on one screen.
const shareSubjectsMax = 50

// The owner's sharing modes, spelled as setWorkerSharing's enum spells them.
// SharingModePeople sits beside the two fleet_machine_acts.go already names.
const SharingModePeople = "people"

// shareEntry is one person or group as the directory renders it.
type shareEntry struct {
	Id      string // bare, the client contract
	Name    string
	Detail  string // an email, and only for a caller who already sees one
	Members int    // groups only: active people with an active membership
}

// shareDirectory is what one owner may pick, plus every active person and
// group by bare id so a stored subject can still be NAMED after it stops
// being offered.
type shareDirectory struct {
	everyone      bool
	people        []shareEntry
	groups        []shareEntry
	offeredPeople map[string]struct{}
	offeredGroups map[string]struct{}
	knownPeople   map[string]shareEntry
	knownGroups   map[string]shareEntry
}

func (d shareDirectory) offersPerson(id string) bool {
	_, ok := d.offeredPeople[BareShortId(strings.TrimSpace(id))]
	return ok
}

func (d shareDirectory) offersGroup(id string) bool {
	_, ok := d.offeredGroups[BareShortId(strings.TrimSpace(id))]
	return ok
}

// actorSeesEveryone reports whether the caller may already read every person
// on the cluster: `read` on `principal`, decided by auth.CapableFor -- the one
// resolver every other gate asks, with role, group and person grants folded in.
//
// A CAPABILITY, NOT A RANK. The owner's rule was "anyone who can already see
// everyone", and a rank floor answered a different question: an admin with an
// explicit deny on reading people, or a custom role ranked above admin that
// was never given the verb, would have been handed every name and email the
// grant withholds from them everywhere else. By default the answer is the
// same -- owner, developer and admin hold `read principal` by seed.
//
// BORROWED AND SYNTHETIC AUTHORITY SEES CO-MEMBERS ONLY. An Unranked context
// is server-side Go acting for somebody (D4), and answering "everyone" for it
// would hand a roster to whatever borrowed the authority.
func (e *MemQLEngine) actorSeesEveryone(ctx context.Context) bool {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.Unranked || ac.Synthetic {
		return false
	}
	subject, ok := e.subjectFor(ctx)
	if !ok || strings.TrimSpace(subject.UserId) == "" {
		return false
	}
	return auth.CapableFor(ctx, subject, auth.VerbRead, auth.ResourcePrincipal)
}

// shareDirectoryFor builds the caller's directory.
func (e *MemQLEngine) shareDirectoryFor(ctx context.Context) (shareDirectory, error) {
	dir := shareDirectory{
		everyone:      e.actorSeesEveryone(ctx),
		offeredPeople: map[string]struct{}{},
		offeredGroups: map[string]struct{}{},
		knownPeople:   map[string]shareEntry{},
		knownGroups:   map[string]shareEntry{},
	}
	caller := BareShortId(strings.TrimSpace(actingUserFromContext(ctx)))
	if caller == "" {
		// No caller is nobody to lend on behalf of. The builtin has already
		// refused this through modelPullMachineFor; answering empty here keeps
		// a future caller of this function on the narrowing side.
		return dir, nil
	}
	rows, err := e.readShareDirectoryRows(ctx)
	if err != nil {
		return dir, err
	}

	type person struct {
		entry  shareEntry
		active bool
	}
	people := map[string]person{}
	activeGroups := map[string]shareEntry{}
	members := map[string][]string{} // group -> people with an ACTIVE membership
	for i := range rows {
		payload := accountRowPayload(rows[i])
		if payload == nil {
			continue
		}
		id := BareShortId(strings.TrimSpace(rows[i].ID))
		if id == "" {
			continue
		}
		switch rows[i].Concept {
		case conceptIdentityUser:
			entry := shareEntry{Id: id, Name: sharePersonName(payload)}
			if email := strings.TrimSpace(stringFromAny(payload["primaryEmail"])); email != "" && dir.everyone {
				entry.Detail = email
			}
			active := payload["active"] == true && strings.TrimSpace(stringFromAny(payload["suspendedAt"])) == ""
			people[id] = person{entry: entry, active: active}
			dir.knownPeople[id] = entry
		case conceptIdentityGroup:
			entry := shareEntry{Id: id, Name: strings.TrimSpace(stringFromAny(payload["name"]))}
			if entry.Name == "" {
				entry.Name = "Unnamed group"
			}
			dir.knownGroups[id] = entry
			if strings.TrimSpace(stringFromAny(payload["status"])) == "active" {
				activeGroups[id] = entry
			}
		case conceptIdentityGroupMembership:
			if strings.TrimSpace(stringFromAny(payload["status"])) != "active" {
				continue
			}
			group := BareShortId(strings.TrimSpace(stringFromAny(payload["groupId"])))
			user := BareShortId(strings.TrimSpace(stringFromAny(payload["userId"])))
			if group != "" && user != "" {
				members[group] = append(members[group], user)
			}
		}
	}

	// The count a group card shows: ACTIVE people with an active membership.
	// A deactivated account cannot sign in, so it cannot make a call the
	// machine would serve, and counting it would overstate who is lent to.
	countActive := func(group string) int {
		n := 0
		for _, user := range members[group] {
			if people[user].active {
				n++
			}
		}
		return n
	}

	offerPerson := func(user string) {
		p, ok := people[user]
		if !ok || !p.active || user == caller {
			return
		}
		if _, dup := dir.offeredPeople[user]; dup {
			return
		}
		dir.offeredPeople[user] = struct{}{}
		dir.people = append(dir.people, p.entry)
	}
	offerGroup := func(group string) {
		entry, ok := activeGroups[group]
		if !ok {
			return
		}
		if _, dup := dir.offeredGroups[group]; dup {
			return
		}
		entry.Members = countActive(group)
		dir.offeredGroups[group] = struct{}{}
		dir.groups = append(dir.groups, entry)
	}

	if dir.everyone {
		for user := range people {
			offerPerson(user)
		}
		for group := range activeGroups {
			offerGroup(group)
		}
	} else {
		// The caller's own ACTIVE groups: an active membership in an active
		// group, the answer activeGroupIdsForUser gives the account scope and
		// the grant resolver -- so the dialog offers exactly the groups whose
		// shares the resolver would honour for this person.
		for group, users := range members {
			if _, active := activeGroups[group]; !active {
				continue
			}
			for _, user := range users {
				if user == caller {
					offerGroup(group)
					for _, colleague := range users {
						offerPerson(colleague)
					}
					break
				}
			}
		}
	}

	sortShareEntries(dir.people)
	sortShareEntries(dir.groups)
	return dir, nil
}

// sharePersonName is the name a person is offered under: the display name,
// then first and last, then a placeholder -- never their email, which a plain
// user is not entitled to read.
func sharePersonName(payload map[string]any) string {
	if name := strings.TrimSpace(stringFromAny(payload["displayName"])); name != "" {
		return name
	}
	first := strings.TrimSpace(stringFromAny(payload["firstName"]))
	last := strings.TrimSpace(stringFromAny(payload["lastName"]))
	if full := strings.TrimSpace(first + " " + last); full != "" {
		return full
	}
	return "Unnamed person"
}

func sortShareEntries(entries []shareEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if a != b {
			return a < b
		}
		return entries[i].Id < entries[j].Id
	})
}

// staged-data: MUST-NOT-GATE -- the directory must offer exactly the
// memberships activeGroupIdsForUser honours for the resolver (that read is
// MUST-NOT-GATE too, for the same rows). Gating here while the resolver does
// not would hide an in-force group share from its owner's dialog -- the group
// still admits people on every call, but the owner could neither see it on
// their machine nor re-save the list without fleetSetSharing refusing the
// group as one they may not pick.
//
// readShareDirectoryRows reads the newest version of every user, group and
// membership row in ONE statement. DISTINCT ON (id) is the newest-version
// collapse the membership reads perform: rows are append-only, so a removal
// is a new version, and without the collapse the original `active` version of
// a removed membership would still place somebody.
func (e *MemQLEngine) readShareDirectoryRows(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("fleetShareDirectory: no database")
	}
	var nodes []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&nodes).
		DistinctOn("id").
		Where("concept IN (?)", bun.In([]string{conceptIdentityUser, conceptIdentityGroup, conceptIdentityGroupMembership})).
		OrderExpr(`id ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("fleetShareDirectory: read people and groups: %w", err)
	}
	return nodes, nil
}

// evaluateFleetShareDirectoryExpression serves the `fleetShareDirectory`
// builtin.
func (e *MemQLEngine) evaluateFleetShareDirectoryExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	registrationId := strings.TrimSpace(stringArg(args, "registrationId"))
	machine, err := e.modelPullMachineFor(ctx, registrationId)
	if err != nil {
		return nil, err
	}
	dir, err := e.shareDirectoryFor(ctx)
	if err != nil {
		return nil, err
	}

	people := make([]any, 0, len(dir.people))
	for _, p := range dir.people {
		people = append(people, map[string]any{"id": p.Id, "name": p.Name, "detail": p.Detail})
	}
	groups := make([]any, 0, len(dir.groups))
	for _, g := range dir.groups {
		groups = append(groups, map[string]any{"id": g.Id, "name": g.Name, "members": g.Members})
	}

	// THE LIST ALREADY ON THE MACHINE, named whether or not it is still
	// offered, so the owner can see who they lent it to and remove somebody
	// who has since left their groups. `known` false is a subject no row
	// names any more -- shown and removable, never guessed at.
	currentPeople := make([]any, 0, len(machine.SharedUserIds))
	for _, id := range machine.SharedUserIds {
		bare := BareShortId(id)
		entry, known := dir.knownPeople[bare]
		currentPeople = append(currentPeople, map[string]any{
			"id": bare, "name": entry.Name, "known": known, "inDirectory": dir.offersPerson(bare),
		})
	}
	currentGroups := make([]any, 0, len(machine.SharedGroupIds))
	for _, id := range machine.SharedGroupIds {
		bare := BareShortId(id)
		entry, known := dir.knownGroups[bare]
		currentGroups = append(currentGroups, map[string]any{
			"id": bare, "name": entry.Name, "known": known, "inDirectory": dir.offersGroup(bare),
		})
	}

	return singleVirtualRow(ShareDirectoryConcept, registrationId, map[string]any{
		"machineId": registrationId,
		"everyone":  dir.everyone,
		"people":    people,
		"groups":    groups,
		"current":   map[string]any{"people": currentPeople, "groups": currentGroups},
	})
}

// normalizeShareIds is the WRITE side: trimmed, empties dropped, reduced to
// the BARE id -- the client contract's spelling, whatever the caller sent --
// one entry per subject, and any id naming one of `skip` dropped (the
// machine's owner, whose own machine is theirs already). Always non-nil: an
// empty list renders as `[]`, never `null`.
//
// BARE, NOT AS SENT. A caller that sent `v1:identity:group:<a user's id>` in
// userIds would otherwise have that spelling stored for a person, validated
// on its bare id and forever read as belonging to a concept it does not.
func normalizeShareIds(ids []string, skip ...string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, s := range skip {
		if bare := BareShortId(strings.TrimSpace(s)); bare != "" {
			seen[bare] = struct{}{}
		}
	}
	for _, raw := range ids {
		bare := BareShortId(strings.TrimSpace(raw))
		if bare == "" {
			continue
		}
		if _, dup := seen[bare]; dup {
			continue
		}
		seen[bare] = struct{}{}
		out = append(out, bare)
	}
	return out
}

// ParseMachineSharing reads a registration's stored `sharing` block the way
// component/worker.SharingFromRow does, for the builtins on this side of the
// import edge (component/worker reaches this package, so it cannot be
// imported here). The lists come back only under `people`, and a `people`
// share naming nobody reads as `owner`. integrations/agent/worker imports both
// and holds the two readings together with a parity test, because two
// readers of one consent that disagreed would lend a machine on one path and
// not on another.
func ParseMachineSharing(v any) (mode string, userIds, groupIds []string) {
	row, ok := v.(map[string]any)
	if !ok || len(row) == 0 {
		return SharingModeOwner, nil, nil
	}
	mode = strings.TrimSpace(stringFromAny(row["mode"]))
	if mode != SharingModeCluster && mode != SharingModePeople {
		return SharingModeOwner, nil, nil
	}
	if mode == SharingModeCluster {
		return mode, nil, nil
	}
	userIds = readShareIdList(row["userIds"])
	groupIds = readShareIdList(row["groupIds"])
	if len(userIds) == 0 && len(groupIds) == 0 {
		return SharingModeOwner, nil, nil
	}
	return mode, userIds, groupIds
}

// readShareIdList is the READ side, and it matches the worker's reader rule for
// rule: a LIST only (a single string where a list belongs is a hand-edited row,
// and neither reader guesses at it), trimmed, empties dropped, one entry per
// subject by BareShortId with the first spelling kept, and nil when empty.
func readShareIdList(v any) []string {
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
	var out []string
	seen := map[string]struct{}{}
	for _, id := range raw {
		id = strings.TrimSpace(id)
		bare := BareShortId(id)
		if bare == "" {
			continue
		}
		if _, dup := seen[bare]; dup {
			continue
		}
		seen[bare] = struct{}{}
		out = append(out, id)
	}
	return out
}

// containsShareSubject reports whether a list names the subject in either
// spelling.
func containsShareSubject(list []string, id string) bool {
	want := BareShortId(strings.TrimSpace(id))
	if want == "" {
		return false
	}
	for _, have := range list {
		if BareShortId(strings.TrimSpace(have)) == want {
			return true
		}
	}
	return false
}
