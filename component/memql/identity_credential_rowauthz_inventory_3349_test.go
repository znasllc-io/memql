package memql

import (
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// identity_credential_rowauthz_inventory_3349_test.go -- memql#3349.
//
// `v1:identity:identity` is the CREDENTIAL concept, and it declares no
// `@rowAuthz` tier. memql#3349 asked for three things: every read
// inventoried with the pre-actor paths named explicitly, a tier declared
// or a recorded decision that one cannot be, and the two account-token
// entries removed from the undeclared list.
//
// This file is the inventory, and it is a TEST rather than a document so
// it cannot quietly go stale. The population is derived from the loaded
// function registry -- never from a scan of the .memql source -- which is
// the memql#2875 lesson the sibling gates already apply: the loader
// already carries which construct binds which concept, and a second
// hand-maintained copy is a second thing to drift.
//
// # THE ANSWER: THE CONCEPT CANNOT CARRY AN `owner` TIER
//
// Not "has not yet". Three independent blockers, each MEASURED by
// declaring `@rowAuthz(owner="userId")` on the concept and running the
// existing gates:
//
//  1. THE OWNER FIELD IS NOT AN OWNERSHIP CLAIM.
//     TestDeclaredOwnerFieldsAreServerStamped reports SEVEN mutations
//     writing `userId` from caller args and ZERO stamping it from the
//     actor: createBadgeIdentity, createIdentity, createNodeTokenIdentity,
//     createPATIdentity, createVoiceAgentTokenIdentity,
//     createWorkerTokenIdentity, updateIdentity.
//
//     That is not an oversight to fix by re-stamping, which is the gate's
//     usual advice. It is what the field MEANS here: an admin mints a PAT
//     or a worker token or a badge FOR another user, and a node token's
//     `userId` is a machine binding chosen by the bootstrap handler. The
//     field names the credential's SUBJECT, not a caller who owns the
//     row. Stamping it from `actor.userId` would break admin issuance,
//     which is the point of the field.
//
//  2. THE PRE-ACTOR READS. Four `*ByKeyHash` lookups are how the actor
//     gets BUILT -- they run on the gRPC interceptor hot path, before any
//     AccessContext exists. `component/identity/pat/verifier.go` states
//     it in the tree's own words: "This is a pre-actor read, so it cannot
//     be caller-scoped (#2800, #2984)."
//
//     `enforceRowAuthzOnPlan` takes NO context and has no escape -- the
//     write path's `rowAuthzWriteEscape` has no read-side counterpart --
//     so a tier ANDs `userId == actor.userId` into credential
//     verification itself, comparing against the empty string. Every PAT,
//     worker-token, badge and node-token login fails.
//
//  3. THE CONCEPT IS A UNION OF CREDENTIAL KINDS WITH DIFFERENT SUBJECTS.
//     `identityType` spans eight variants, and the machine ones
//     (node_token, voice_agent_token) authenticate a PROCESS, not a
//     person. `nodeTokenIdentities` is an operator listing of every node
//     credential in the cluster and is caller-scopable by nobody.
//
// Measured breakage on the read side, from
// TestRowAuthzEnforcementLandGate with the tier declared: NINE of the
// eleven reads go undecidable, each reported as "Enforcement ANDs
// userId==actor.userId into every read of this concept, so this
// construct's result set CHANGES for its callers."
//
// # WHAT WOULD ACTUALLY CLOSE IT
//
// In dependency order, and none of them is a one-line annotation:
//
//	(a) A READ-PATH ESCAPE mirroring rowAuthzWriteEscape -- internal
//	    origin, stamped per-call by an allow-listed package -- so the
//	    pre-actor verification reads survive injection. Every one of them
//	    already stamps it (pat/store.go, workertoken/store.go,
//	    badge/store.go, identity/store.go), so the discipline exists; the
//	    read path simply cannot consult it today.
//	(b) A SPLIT OF THE CONCEPT BY CREDENTIAL KIND, so the person-owned
//	    credentials (api_key, worker_token, badge, magic_link, oauth) can
//	    carry `owner="userId"` while the machine credentials
//	    (node_token, voice_agent_token, service_account) carry
//	    `clusterOwner`. This is the option memql#3349 itself floats, and
//	    it is a data migration plus a wire-contract change across every
//	    caller above, not a bug fix.
//
// Until one of those lands, the eleven entries stay on the undeclared
// list and this file is what keeps them honest.
//
// # THE THIRD ACCEPTANCE BOX
//
// "The two account-token entries removed from the undeclared list."
// `accountTokenById` and `accountTokensForAccount` were added by
// memql#3322, which lives on the UNMERGED `epic/memql-portal` branch.
// Neither the queries nor the list entries exist on main -- verified by
// TestAccountTokenEntriesAreNotOnMain below. There is nothing to remove
// here; when that epic merges, its two entries join the eleven and are
// covered by the same finding.

const credentialConcept = "v1:identity:identity"

// credentialReadClass is why one read over the credential concept cannot
// simply carry an ownership conjunct.
type credentialReadClass string

const (
	// classPreActor -- runs BEFORE an AccessContext exists, because it is
	// how the actor is resolved. `actor.userId` is circular here.
	classPreActor credentialReadClass = "pre-actor"
	// classSelfScoped -- already filters `userId==actor.userId`. These are
	// the two memql#3178 added; they are strictly narrower than what they
	// replaced and are blocked only by the concept, not by themselves.
	classSelfScoped credentialReadClass = "self-scoped"
	// classAdminScoped -- a deliberate cross-user read behind a role gate.
	// An owner tier would narrow it to the admin's own rows.
	classAdminScoped credentialReadClass = "admin-scoped"
	// classMachine -- reads a credential that authenticates a PROCESS.
	// There is no person whose actor.userId could scope it.
	classMachine credentialReadClass = "machine-credential"
)

// credentialReadInventory pins every construct binding the credential
// concept, with the reason a tier does not reach it.
//
// memql#3349 acceptance box 1. Adding a read means classifying it here,
// which is the point: the next query over this concept starts from a
// stated position rather than from zero.
var credentialReadInventory = map[string]struct {
	class  credentialReadClass
	reason string
}{
	// --- PRE-ACTOR: the reads that BUILD the actor. ---
	"patIdentityByKeyHash": {classPreActor,
		"PAT verification. pat/store.go LookupByKeyHash, described in-tree as " +
			"'the gRPC-interceptor hot path'; pat/verifier.go states the read is pre-actor " +
			"and cannot be caller-scoped (#2800, #2984)."},
	"workerTokenByKeyHash": {classPreActor,
		"Worker-token verification on WorkerService.Stream. workertoken/store.go; " +
			"resolves mql_wkr_<...> to its identity before any actor exists."},
	"badgeByKeyHash": {classPreActor,
		"Badge grant exchange. badge/store.go. The TERMINAL is authenticated, but the " +
			"badge holder is a different subject, so the terminal's actor.userId is the " +
			"wrong thing to scope by even where an actor exists."},
	"nodeTokenIdentityByBinding": {classPreActor,
		"Node-token verification on NodeService.Stream (@serverOnly). identity/store.go; " +
			"authenticates a node binary by (nodeType, nodeId) before it has any identity."},

	// --- SELF-SCOPED: already narrowed, blocked only by the concept. ---
	"patIdentitiesForSelf": {classSelfScoped,
		"filter userId==actor.userId (memql#3178). Strictly narrower than the " +
			"userId==args.userId construct it replaced."},
	"badgesForSelf": {classSelfScoped,
		"filter userId==actor.userId (memql#3178). Same shape as patIdentitiesForSelf."},
	"accountTokensForAccount": {classSelfScoped,
		"filter identityType==\"account_token\" && userId==actor.userId && " +
			"credentials.accountId==args.accountId (memql#3322). An account token's SUBJECT is " +
			"the calling operator and the account is a binding on it, so the self-scope " +
			"conjunct is the real narrowing; accountId is a further filter, not the " +
			"authorization."},
	"accountTokenById": {classSelfScoped,
		"filter row.id==args.identityId && identityType==\"account_token\" && " +
			"userId==actor.userId (memql#3322). Strictly stronger than patIdentityById, the " +
			"weakest entry below: that one resolves a credential by id with no user-scope " +
			"conjunct at all, where this one carries its own."},

	// --- ADMIN-SCOPED: deliberate cross-user reads behind a role gate. ---
	"patIdentitiesForUser": {classAdminScoped,
		"filter userId==args.userId && requiresOwnerOrAdmin -- an admin listing ANOTHER " +
			"user's PATs. An owner tier would narrow it to the admin's own and return a " +
			"confidently wrong answer."},
	"patIdentityById": {classAdminScoped,
		"Resolves one PAT row by id for the revoke/inspect path. Carries no user-scope " +
			"conjunct of its own -- the weakest entry in this inventory, and the one most " +
			"worth revisiting first if the concept is ever split."},
	"workerTokensForUser": {classAdminScoped,
		"@serverOnly, filter userId==args.userId. Reached through workertoken/store.go " +
			"ListForUser under internal origin (the memql#3072 per-call stamp)."},

	// --- MACHINE CREDENTIALS: no person to scope by. ---
	"nodeTokenIdentities": {classMachine,
		"@serverOnly operator listing of EVERY node credential in the cluster. There is " +
			"no requesting user whose actor.userId could scope it; the audience is the " +
			"operator, which is a clusterOwner question, not an owner one."},
	"nodeTokenIdentityById": {classMachine,
		"@serverOnly single node credential by id, for the revoke/inspect path. Same " +
			"audience as nodeTokenIdentities."},
}

// TestCredentialConceptReadInventory pins the population (box 1).
//
// Derived from the loaded registry so a new read over the credential
// concept fails here until it is classified, and a deleted one fails
// until its entry goes -- the shrink-only discipline the sibling gate
// states as "a stale exemption is as bad as a missing gate".
func TestCredentialConceptReadInventory(t *testing.T) {
	conceptCount, conceptSkips, err := LoadUnifiedConceptsWithSkips(nil)
	if err != nil {
		t.Fatalf("LoadUnifiedConceptsWithSkips: %v", err)
	}
	if len(conceptSkips) > 0 {
		t.Fatalf("%d concept(s) failed to load, so this inventory cannot see them: %v",
			len(conceptSkips), conceptSkips)
	}
	if conceptCount == 0 {
		t.Fatal("no concepts loaded")
	}

	registry := newFunctionRegistry()
	report := newLoadReport()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), report); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("%d construct(s) were skipped at load, so this inventory would report them "+
			"as gone:\n  %v", len(report.Skipped), report.Skipped)
	}

	world := deriveUndeclaredWorld(registry)
	if len(world.queryConcept) == 0 {
		t.Fatal("the derivation saw no query constructs -- a broken run, not an empty concept")
	}

	live := map[string]bool{}
	for construct, concept := range world.queryConcept {
		if concept == credentialConcept {
			live[construct] = true
		}
	}
	if len(live) == 0 {
		t.Fatalf("no construct binds %s. Either the concept was renamed or the derivation "+
			"broke; do not empty this inventory on that signal alone.", credentialConcept)
	}

	var unclassified, stale []string
	for construct := range live {
		if _, ok := credentialReadInventory[construct]; !ok {
			unclassified = append(unclassified, construct)
		}
	}
	for construct := range credentialReadInventory {
		if !live[construct] {
			stale = append(stale, construct)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)

	if len(unclassified) > 0 {
		t.Errorf(`%d new read(s) over %s are not in the memql#3349 inventory:

  %s

Classify each one. The concept declares no @rowAuthz tier and cannot carry an owner tier
(see this file's header for the three measured blockers), so the engine measures NOTHING
about your read -- its filter is the only thing scoping it, and that is exactly what
memql#3349 exists to stop relying on silently.

State which class it is:
  pre-actor          -- it runs before an AccessContext exists (it builds the actor)
  self-scoped        -- it filters userId==actor.userId
  admin-scoped       -- a deliberate cross-user read behind a role gate
  machine-credential -- it reads a node/voice-agent credential; no person to scope by

If it is none of those, it is probably a read that SHOULD be caller-scoped, and the fix is
the conjunct rather than an entry here.`,
			len(unclassified), credentialConcept, strings.Join(unclassified, "\n  "))
	}

	if len(stale) > 0 {
		t.Errorf(`%d inventory entry(ies) name a construct that no longer binds %s:

  %s

Delete them. A stale entry suppresses the question for whoever writes the next query.`,
			len(stale), credentialConcept, strings.Join(stale, "\n  "))
	}
}

// The pre-actor set is named EXPLICITLY, which is what memql#3349 asked
// for by name. It is also the set that decides the whole question: these
// are the reads no tier can be given an escape from today, because
// enforceRowAuthzOnPlan takes no context.
func TestCredentialPreActorReadsAreNamed(t *testing.T) {
	want := []string{
		"badgeByKeyHash",
		"nodeTokenIdentityByBinding",
		"patIdentityByKeyHash",
		"workerTokenByKeyHash",
	}

	var got []string
	for construct, entry := range credentialReadInventory {
		if entry.class == classPreActor {
			got = append(got, construct)
		}
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf(`the pre-actor set changed.

  was:  %v
  now:  %v

This set is load-bearing for the memql#3349 decision: a tier is refused BECAUSE these reads
resolve the caller's identity and therefore cannot compare against it. If the set SHRANK,
re-ask whether a tier is now declarable. If it GREW, a new credential-verification path was
added and it needs the same scrutiny.`, want, got)
	}
}

// The concept still declares no tier.
//
// If someone declares one, the eleven reads above change behaviour and
// credential verification is in the blast radius. Fail loudly with the
// measurements rather than let it land quietly.
func TestCredentialConceptStillDeclaresNoTier(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memoryNodes.Get(credentialConcept)
	if err != nil || c == nil {
		t.Fatalf("concept %q is not loaded: %v", credentialConcept, err)
	}
	if c.RowAuthz != nil {
		t.Fatalf(`%s now declares @rowAuthz(%v).

memql#3349 recorded that it CANNOT carry an owner tier, with three measured blockers:

  1. SEVEN mutations write `+"`userId`"+` from caller args and ZERO stamp it from the actor.
     The field names the credential's SUBJECT (an admin mints a PAT for another user; a
     node token's userId is a machine binding), not a caller who owns the row.
  2. FOUR pre-actor reads BUILD the actor. enforceRowAuthzOnPlan has no context and no
     escape, so a tier ANDs userId==actor.userId into credential verification itself and
     every PAT / worker-token / badge / node-token login fails.
  3. The concept is a UNION of eight credential kinds; the machine ones authenticate a
     process, not a person.

Before landing a tier here, confirm: does TestDeclaredOwnerFieldsAreServerStamped pass?
Does TestRowAuthzEnforcementLandGate still report zero would-narrow? Can a PAT still
authenticate? If the answer to any is no, this is the regression this test exists to catch.`,
			credentialConcept, c.RowAuthz)
	}
}

// Every inventory entry carries a reason. An entry without one records a
// classification nobody made -- the same rule the undeclared gate's
// reason slot enforces (memql#3135).
func TestCredentialInventoryEntriesCarryReasons(t *testing.T) {
	for construct, entry := range credentialReadInventory {
		if strings.TrimSpace(entry.reason) == "" {
			t.Errorf("%s has no reason", construct)
		}
		switch entry.class {
		case classPreActor, classSelfScoped, classAdminScoped, classMachine:
		default:
			t.Errorf("%s carries an unknown class %q", construct, entry.class)
		}
	}
}

// The inventory and the undeclared list agree.
//
// Two lists over one concept is how they drift. Every construct pinned
// here must also be on the undeclared list, because the concept declares
// no tier -- if that stops being true, one of the two is wrong.
func TestCredentialInventoryMatchesTheUndeclaredList(t *testing.T) {
	var missing []string
	for construct := range credentialReadInventory {
		entry, ok := undeclaredRowAuthzConstructs[construct]
		if !ok {
			missing = append(missing, construct+" (absent from the undeclared list)")
			continue
		}
		if entry.concept != credentialConcept {
			missing = append(missing, construct+" (undeclared list binds it to "+entry.concept+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf(`the memql#3349 inventory and the undeclared list disagree:

  %s

Both describe reads over %s that the engine does not measure. They must name the same set.`,
			strings.Join(missing, "\n  "), credentialConcept)
	}
}

// memql#3349 acceptance box 3, answered -- and the handshake it left has
// now fired.
//
// The issue asked for "the two account-token entries removed from the
// undeclared list". When #3349 landed they existed only on the unmerged
// epic/memql-portal branch, so its test asserted their ABSENCE and told
// the epic what to do when it arrived: classify them in the inventory
// above, and leave the undeclared entries in place, because removing an
// entry means the CONCEPT declares a tier and this one cannot (see the
// three measured reasons in the file header).
//
// memql#3322 has since landed and that instruction was followed. So the
// assertion inverts: both constructs must now be BOTH inventoried and
// still on the undeclared list. Dropping either half is the regression
// this guards -- inventoried-but-not-listed would mean the concept
// silently grew a tier, and listed-but-not-inventoried would be a read
// slipping in as "already accounted for", which is exactly what the
// original test existed to prevent.
func TestAccountTokenReadsAreClassifiedAndStillUnmeasured(t *testing.T) {
	for _, construct := range []string{"accountTokenById", "accountTokensForAccount"} {
		if _, ok := credentialReadInventory[construct]; !ok {
			t.Errorf(`%s reads %s but is not classified in credentialReadInventory.

A read over the credential concept starts from a STATED position or not at all -- that is
what memql#3349 box 1 bought. Classify it beside the other reads.`, construct, credentialConcept)
		}
		if _, ok := undeclaredRowAuthzConstructs[construct]; !ok {
			t.Errorf(`%s left the undeclared list, which claims %s now declares an @rowAuthz tier.

If that is genuinely true, this whole file is stale and should go with it. If it is not,
the entry was deleted to quiet a gate -- and that suppresses the construct forever while
the concept stays unmeasured.`, construct, credentialConcept)
		}
	}
}
