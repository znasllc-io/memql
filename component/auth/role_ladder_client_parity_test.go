package auth

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ONE ROLE LADDER, PINNED ACROSS THE LANGUAGES THAT READ IT (epic
// memql#4832, task memql#4833).
//
// Two hand-maintained ladders existed and disagreed. The engine ranked
// developer 300 above admin 200; MemQL OS ranked admin above developer. The
// only symptom for the life of both files was a mis-sorted launcher -- and
// under rank-based row visibility the same request would have got opposite
// answers depending on which side answered it.
//
// WHAT THIS GATE ASSERTS, AND WHY IT IS SHAPED THIS WAY.
//
// It is NOT "the two orderings match". That was available and is the wrong
// gate: it would have kept two copies and merely required them to agree, which
// is the arrangement that produced the bug -- both sides kept working, they
// just disagreed. The shell now holds NO ordering, so what is checked is that
// it still holds none, plus that the one remaining copy in the test tree
// matches the seeds it stands in for.
//
// A MISSING FILE IS A FAILURE, NOT A SKIP. A skip would make this gate
// silently vacuous the moment a file were renamed or moved -- which is exactly
// when the rule is most likely to have drifted. The failure names the path so
// the fix is obvious in either direction. This is the rule
// TestFleetOnlineWindowMatchesPortal already states for its own three copies.

const (
	osRolesPath  = "../../clients/os/src/system/roles.ts"
	osLadderPath = "../../clients/os/test/seededLadder.ts"
	rbacSeedPath = "../../dsl/rbac/seeds.memql"
)

// clientLadderLiteralPattern finds an ARRAY OF ROLE SLUGS -- the shape the
// deleted `ROLE_LADDER` had.
//
// It matches on the CONTENT (two or more known role names in one bracketed
// list) rather than on the name `ROLE_LADDER`, because the failure this
// prevents is somebody reintroducing the ordering under a different name --
// `RANKS`, `LADDER`, an inline literal inside roleRank. Keying on the old
// identifier would have caught only the one spelling nobody would use twice.
var clientLadderLiteralPattern = regexp.MustCompile(
	`\[[^\]\n]*"(?:owner|admin|developer|writer|reader|user|viewer)"[^\]\n]*,[^\]\n]*"(?:owner|admin|developer|writer|reader|user|viewer)"[^\]\n]*\]`)

// TestClientShipsNoRoleOrdering fails if MemQL OS carries an ordering of its
// own again.
func TestClientShipsNoRoleOrdering(t *testing.T) {
	src := readClientFile(t, osRolesPath)

	if m := clientLadderLiteralPattern.FindString(stripComments(src)); m != "" {
		t.Fatalf("%s contains what looks like a hardcoded role ordering: %s\n\n"+
			"The ladder is CLUSTER STATE (epic memql#4832, D1). Two hand-maintained ladders is\n"+
			"the defect this closed -- the engine ranked developer above admin and the shell\n"+
			"ranked admin above developer, and nothing noticed for the life of both files.\n"+
			"A literal here is that arrangement returning, and a literal kept as a FALLBACK is\n"+
			"the same thing behind a condition nobody exercises.\n\n"+
			"Read the ordering from `activeRoles` (see useRoleLadder.ts) instead.",
			osRolesPath, m)
	}

	// THE REACHABLE POSITIVE. Without it this gate passes against an empty
	// file, a deleted predicate, or a roles.ts that no longer gates anything
	// -- every one of which is a worse state than the literal it refuses.
	for _, required := range []string{"setRoleLadder", "roleAdmits", "roleRank"} {
		if !strings.Contains(src, required) {
			t.Fatalf("%s no longer defines %s.\n"+
				"This gate refuses a hardcoded ordering in that file; if the file has stopped\n"+
				"carrying the role predicate at all, the gate is passing while measuring nothing.",
				osRolesPath, required)
		}
	}
}

// TestClientLadderFixtureMatchesTheSeeds pins the ONE remaining copy of the
// ladder -- the OS test fixture -- against dsl/rbac/seeds.memql.
//
// The fixture has to exist: jsdom has no cluster to read a ladder from. So the
// copy is not avoidable, only pinned -- and an unpinned copy of an ordering is
// precisely what this epic deleted from the shell.
func TestClientLadderFixtureMatchesTheSeeds(t *testing.T) {
	seeded := seededRungs(t)
	fixture := fixtureRungs(t)

	if len(seeded) == 0 {
		t.Fatalf("parsed no roles out of %s -- this gate would pass by comparing two empty sets",
			rbacSeedPath)
	}
	if len(fixture) != len(seeded) {
		t.Fatalf("%s declares %d rungs, %s seeds %d.\nfixture=%v\nseeds=%v",
			osLadderPath, len(fixture), rbacSeedPath, len(seeded), fixture, seeded)
	}
	for slug, rank := range seeded {
		got, ok := fixture[slug]
		if !ok {
			t.Fatalf("%s seeds role %q and %s does not carry it", rbacSeedPath, slug, osLadderPath)
		}
		if got != rank {
			t.Fatalf("role %q ranks %d in %s and %d in %s -- one ladder, two numbers",
				slug, rank, rbacSeedPath, got, osLadderPath)
		}
	}
}

// TestEngineRankModelMatchesTheSeeds pins the GO mirror against the same
// seeds, so all three readings of the ladder agree.
//
// component/auth/rbac_model.go is a hand-written mirror of the seed rows, and
// it is what every Go gate resolves a rank through. It is the copy whose
// divergence would be least visible: no screen renders it.
func TestEngineRankModelMatchesTheSeeds(t *testing.T) {
	seeded := seededRungs(t)
	// The legacy slugs the user row carries, mapped to the rung each aliases.
	// Asserted through RoleRank so this measures the function every gate
	// actually calls rather than the table behind it.
	for role, slug := range map[Role]string{
		RoleOwner:     "owner",
		RoleDeveloper: "developer",
		RoleAdmin:     "admin",
		RoleWriter:    "user",
		RoleReader:    "viewer",
	} {
		want, ok := seeded[slug]
		if !ok {
			t.Fatalf("%s no longer seeds role %q, which auth.%s maps to", rbacSeedPath, slug, role)
		}
		if got := RoleRank(role); got != want {
			t.Fatalf("auth.RoleRank(%q) = %d, but %s seeds %q at %d -- the Go mirror and the "+
				"DSL catalog are one ladder and must carry one number",
				role, got, rbacSeedPath, slug, want)
		}
	}

	// THE DECISION ITSELF, pinned as a statement rather than left implicit in
	// the numbers: developer OUTRANKS admin. The table above would pass with
	// both at any pair of values, including the shell's old ordering.
	if RoleRank(RoleDeveloper) <= RoleRank(RoleAdmin) {
		t.Fatalf("developer (%d) does not outrank admin (%d). D1 locked the engine's ordering as "+
			"the only one; flipping it back re-opens the divergence this epic closed",
			RoleRank(RoleDeveloper), RoleRank(RoleAdmin))
	}
}

// ---------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------

func readClientFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v\n"+
			"This gate is the only thing keeping the engine's role ladder and MemQL OS's use of\n"+
			"it in step. If the file moved, update the path const; if it was deleted, the shell\n"+
			"is gating on something else and that needs a decision, not a skip.", path, err)
	}
	return string(raw)
}

// seedRolePattern reads `seed role <name> { ... rank: <n> ... }` out of the
// DSL. Anchored on the seed keyword so a role NAMED in prose is not parsed as
// a declaration.
var seedRolePattern = regexp.MustCompile(`(?s)seed\s+role\s+\w+\s*\{(.*?)\n\}`)
var seedSlugPattern = regexp.MustCompile(`slug:\s*"([^"]+)"`)
var seedRankPattern = regexp.MustCompile(`rank:\s*(\d+)`)

func seededRungs(t *testing.T) map[string]int {
	t.Helper()
	src := readClientFile(t, rbacSeedPath)
	out := map[string]int{}
	for _, block := range seedRolePattern.FindAllStringSubmatch(src, -1) {
		slug := seedSlugPattern.FindStringSubmatch(block[1])
		rank := seedRankPattern.FindStringSubmatch(block[1])
		if slug == nil || rank == nil {
			continue
		}
		n, err := strconv.Atoi(rank[1])
		if err != nil {
			continue
		}
		out[slug[1]] = n
	}
	return out
}

var fixtureRungPattern = regexp.MustCompile(`slug:\s*"([^"]+)"\s*,\s*name:\s*"[^"]*"\s*,\s*rank:\s*(\d+)`)

func fixtureRungs(t *testing.T) map[string]int {
	t.Helper()
	src := readClientFile(t, osLadderPath)
	out := map[string]int{}
	for _, m := range fixtureRungPattern.FindAllStringSubmatch(src, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		out[m[1]] = n
	}
	if len(out) == 0 {
		t.Fatalf("parsed no rungs out of %s -- the fixture's shape changed and this gate is "+
			"comparing against nothing", osLadderPath)
	}
	return out
}

// stripComments blanks line and block comments so a role name discussed in
// prose -- and every one of these files discusses them at length -- cannot be
// read as a declaration.
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return b.String()
			}
			b.WriteByte('\n')
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 1
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------
// The surface requirement, and its presentation mirror (D6)
// ---------------------------------------------------------------------

const osAppRegistryPath = "../../clients/os/src/apps/registry.tsx"

// appFloorMirrors maps an OS app id to the DSL file whose constructs that app
// calls, and the floor both must state.
//
// AN EXPLICIT TABLE, NOT A DERIVATION, and the honesty about that matters. A
// "surface is a set of constructs" is true and is not mechanically recoverable
// from a TSX file: the app calls generated SDK methods, and chasing them to
// their DSL definitions would be a static analysis whose failure mode is
// silence. So the coupling is DECLARED here, which means adding an app with a
// role requirement means adding a line -- and a missing line is a mirror
// nobody checks, which is why the count is asserted below.
var appFloorMirrors = []struct {
	appID     string
	dslFile   string
	construct string
}{
	{appID: "accounts", dslFile: "../../dsl/accounts/queries.memql", construct: "clientAccountsAll"},
}

// TestAppManifestMirrorsTheEngineFloor pins the OS manifest's `roles.min`
// against the `@requiresRank` its constructs declare (epic memql#4832, D6).
//
// The presentation gate and the enforced gate are BOTH permanent and neither
// stands in for the other -- hiding an app somebody cannot reach beats letting
// them open it and read a refusal, and that is a different job from refusing
// it. What must never happen is the two disagreeing, because each failure is
// invisible from the other side: a manifest stricter than the engine hides an
// app people may use, and a manifest looser than the engine offers an app that
// refuses every read inside it.
func TestAppManifestMirrorsTheEngineFloor(t *testing.T) {
	registry := readClientFile(t, osAppRegistryPath)
	for _, m := range appFloorMirrors {
		manifestFloor := appManifestFloor(t, registry, m.appID)
		engineFloor := requiresRankFloor(t, readClientFile(t, m.dslFile), m.construct)
		if manifestFloor != engineFloor {
			t.Fatalf("app %q declares roles.min=%q in %s, and %s declares @requiresRank(%q) in %s.\n"+
				"The manifest is the PRESENTATION MIRROR of the engine's floor; when they disagree "+
				"one of two invisible things is true -- an app people may use is hidden, or an app "+
				"that refuses every read inside it is offered.",
				m.appID, manifestFloor, osAppRegistryPath, m.construct, engineFloor, m.dslFile)
		}
	}

	// THE COUNT. Every app carrying a role requirement needs a mirror entry,
	// or this gate quietly measures a shrinking subset of the registry.
	gated := regexp.MustCompile(`roles:\s*\{\s*min:\s*"[^"]+"\s*\}`).FindAllString(stripComments(registry), -1)
	if len(gated) < len(appFloorMirrors) {
		t.Fatalf("%s declares %d role requirements but appFloorMirrors names %d apps -- "+
			"the table is ahead of the registry", osAppRegistryPath, len(gated), len(appFloorMirrors))
	}
}

func appManifestFloor(t *testing.T, registry, appID string) string {
	t.Helper()
	block := regexp.MustCompile(
		`(?s)const \w+: OsAppManifest = \{\s*\n\s*id: "` + regexp.QuoteMeta(appID) + `",(.*?)\n\};`,
	).FindStringSubmatch(registry)
	if block == nil {
		t.Fatalf("no OsAppManifest with id %q in %s -- if the app was renamed, update "+
			"appFloorMirrors; a mirror pointing at nothing checks nothing", appID, osAppRegistryPath)
	}
	m := regexp.MustCompile(`roles:\s*\{\s*min:\s*"([^"]+)"\s*\}`).FindStringSubmatch(stripComments(block[1]))
	if m == nil {
		t.Fatalf("app %q declares no roles.min, but appFloorMirrors expects it to mirror an "+
			"engine floor. Either add the requirement or drop the mirror entry", appID)
	}
	return m[1]
}

func requiresRankFloor(t *testing.T, dsl, construct string) string {
	t.Helper()
	// The annotation sits ABOVE the construct's signature, so the search is
	// anchored on the signature and walks back over the annotation block.
	idx := regexp.MustCompile(`(?m)^(?:query|mutate|logic)\s+\w+\s+` + regexp.QuoteMeta(construct) + `\s*\{`).FindStringIndex(dsl)
	if idx == nil {
		t.Fatalf("no construct named %q in the DSL file -- if it was renamed, update "+
			"appFloorMirrors", construct)
	}
	head := dsl[:idx[0]]
	// The LAST @requiresRank before the signature is this construct's; an
	// earlier one belongs to a construct further up the file.
	all := regexp.MustCompile(`@requiresRank\("([^"]+)"\)`).FindAllStringSubmatch(head, -1)
	if len(all) == 0 {
		t.Fatalf("%q declares no @requiresRank, but its app's manifest declares a role floor. "+
			"A presentation gate with nothing behind it is the state epic memql#4832 exists to "+
			"end -- the requirement has to be enforced somewhere.", construct)
	}
	return all[len(all)-1][1]
}
