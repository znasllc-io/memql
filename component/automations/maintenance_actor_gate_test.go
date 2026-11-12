package automations

import (
	"context"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/repowalk"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// THE GATE on the cluster's maintenance principal (memql#4366 / memql#4406).
//
// component/auth/maintenance_actor.go holds a compiled-in list of automations
// that run as a synthetic CLUSTER OWNER instead of the ordinary RoleReader
// system actor. That is the largest privilege any automation can hold, and the
// list is one line per grant, so this gate makes an addition a visible, argued
// change rather than an edit nobody has to defend.
//
// Four properties, and each one closes a way the list could quietly go wrong:
//
//  1. Every entry is PINNED here. Adding a grant means editing two files, one
//     of which is a test whose whole subject is why the grant exists.
//  2. Every entry carries a non-empty reason.
//  3. Every entry names an automation that ACTUALLY LOADS from this repo's dsl/
//     tree, and loads from the ENGINE'S layer of it (Automation.Source). That
//     is what "engine-owned" means operationally: a product bundle mounted at
//     MEMQL_DSL_PATH contributes automations at runtime, and a name that
//     resolves to nothing here would be a grant waiting for whoever happens to
//     define it.
//  4. The JOIN holds -- contextWithSystemActor really does yield a cluster
//     owner for the engine's automation of a listed name, and really does not
//     for the same name from any other origin, nor for anything else. The list
//     and the constructor are in a different package from the executor that
//     consumes them, so nothing else asserts that the wire is connected.
func TestMaintenanceAutomationsAreArgued(t *testing.T) {
	// (1) The pinned set.
	//
	// seedSelfAccount joined it in epic memql#4800, and it is the first entry
	// that is not a sweep -- so the second property is worth stating in its
	// own terms rather than by analogy. Its read does not span MANY owners; it
	// reads a row that has NO owner. `v1:accounts:account:self` is the
	// deployment's own company, seeded cluster-owned with `ownerUserId` empty,
	// and sameRowAuthzOwner refuses an empty owner outright -- so the owned
	// branch of the composite tier cannot match it for anybody, and the
	// cluster-owner escape is the only branch that reaches it at all.
	//
	// That is what rules out the alternative this gate points at. Borrowing
	// ONE owner's authority with auth.ContextWithUserActor works when a row
	// has an owner to borrow; here there is none, and no user's envelope --
	// not even the cluster's first user's -- can see the row.
	//
	// What a narrowed read would cost is the sharp end: the automation gates
	// its create on the probe being empty, so a probe that cannot see the row
	// reports "absent" and the seed re-runs on every boot, overwriting the
	// name a human typed into the first-run card. That is decision D3's
	// failure, arrived at through authorization rather than through the gate
	// D3 actually warns about.
	//
	// sweepAbandonedPackageDeployments joined them with epic memql#4900, and it
	// has both properties. It is engine-owned (dsl/platform/automations.memql),
	// and its read spans owners BY NATURE rather than inconveniently: a node
	// that died was running SOMEBODY's deploy and there is nobody left to ask,
	// which is the whole reason the sweep exists -- the person watching a stuck
	// rail is exactly the person who cannot fix it. Borrowing one owner's
	// authority is not available to it, because it does not know whose runs are
	// stranded until it has read them. Under the default reader actor the read
	// answers zero rows and no error, so a sweep that closes nothing would be
	// indistinguishable from a cluster with nothing stranded.
	//
	// logsRetentionSweep joined in epic memql#4893. It is a sweep in the plain
	// sense -- every node's log lines, past their retention -- but the rows
	// it sweeps carry NO row tier: log_line is a dedicated hypertable outside
	// graph admission. What needs the principal is the executor's own floor:
	// builtin logsSweep is reserved to a cluster owner in its Go handler
	// (design L3), and the automation's default RoleReader actor would be
	// refused by the sweep it exists to run.
	// The work spine's two sweeps joined in epic memql#4966. Both are the
	// ordinary reason rather than logsRetentionSweep's: v1:work:run,
	// v1:work:modelCall and v1:work:observation all declare the composite
	// owner tier, so under the default RoleReader actor the owned branch
	// matches nothing, the cluster-owner escape does not apply, and the read
	// answers ZERO ROWS AND NO ERROR. sweepWaitingWorkRuns must see EVERY
	// owner's parked runs -- a person whose run is stranded on a replica that
	// has gone cannot resume it themselves, which is the whole reason it
	// exists -- and workJournalRetentionSweep must fold each run's summary
	// before its detail ages out, so a read that sees nothing does not merely
	// skip a delete, it destroys the evidence it was there to preserve.
	//
	// Procedure learning and the certification ladder added four in epic
	// memql#5408 (closing epic memql#5402's gap G10), and they share ONE
	// argument rather than four: each must read a row BEFORE it can know whose
	// it is. onProcedurePromotionDecided is fired by an event that names an
	// approval, a composite-owner row behind a cluster-owner-scoped by-id read,
	// so the default reader actor sees it as ABSENT and an approval a person
	// gave is applied to nothing. mineProcedureCorpusAcrossAutomations,
	// demoteProcedures and retireProcedures are fired by a schedule, which
	// carries no owner at all, so they list owners and walk each one. What none
	// of them does is read one person's rows as anybody else: every read after
	// the first borrows the owner's own actor, because a procedure mined across
	// two people's recordings is correct about neither. learnFromSucceededRun
	// is absent on purpose: its event carries the owner, which it borrows.
	want := []string{
		"auditEventRetentionSweep",
		"checkDeployableHealth",
		"demoteProcedures",
		// emailStatisticsRollupSweep (the 2026-11-08 statistics design) folds
		// every organization's campaign deliveries, engagement and consent
		// events into the email statistics facts: a cross-owner roll-up by
		// nature, read through @serverOnly queries filtered on
		// actor.isClusterOwner. Under the default RoleReader actor those reads
		// answer zero rows and the builtin refuses every run besides, so every
		// email figure would read as not measured.
		"emailStatisticsRollupSweep",
		// geoCountryRefreshSweep (the 2026-11-08 statistics design) downloads
		// DB-IP's monthly country database into a dedicated table outside
		// graph admission; builtin geoCountryRefresh admits this principal and
		// nobody else, so under the default RoleReader actor every run is
		// refused and the country data is never refreshed.
		"geoCountryRefreshSweep",
		"logsRetentionSweep",
		"mineProcedureCorpusAcrossAutomations",
		"onProcedurePromotionDecided",
		// The engine-owned repository poll discovers sources across owners
		// before borrowing each source owner's credential/deploy authority.
		// A reader actor silently sees no packages and reports checked=0.
		"pollPackageUpstreams",
		// reclaimClosedChunkSpace is logsRetentionSweep's case, not a cross-owner
		// read: the closed-chunk compaction reads no row through the graph (it
		// sums column sizes and rewrites a chunk's storage as it stands), and
		// what needs the principal is its native handler's cluster-owner floor --
		// the pass spends the installation's I/O and WAL. Under the default
		// RoleReader actor it would be refused every night, and the space
		// retention frees inside closed chunks would never come back.
		"reclaimClosedChunkSpace",
		// Declared graph retention spans owners and its native handler has an
		// owner floor, including dry runs. The default scheduled reader is refused.
		"retireDeclaredConceptVersions",
		"retireProcedures",
		// routingEvidenceFold (epic memql#5146) is the one entry here whose
		// read spans owners because the QUESTION does. It asks how a model
		// behaved across the fleet, and a model's behaviour is not one
		// person's to observe: there is no single owner whose authority could
		// be borrowed instead, because borrowing one would answer a narrower
		// question while looking like an answer to this one. It PROPOSES a
		// demotion and applies none, so the privilege buys a read and a parked
		// approval rather than a write anybody has to trust.
		"routingEvidenceFold",
		"seedSelfAccount",
		// siteStatisticsRollupSweep (the 2026-11-08 statistics design) is
		// logsRetentionSweep's case with a cross-owner read besides: it folds
		// every deployable's requests into its statistics in one statement per
		// day, and the rows it reads and writes are dedicated tables outside
		// graph admission. What needs the principal is the builtin's own
		// floor -- siteStatisticsRollup admits this principal and nobody else
		// -- so under the default RoleReader actor every run is refused and
		// every deployable's statistics read as not measured.
		"siteStatisticsRollupSweep",
		"sweepAbandonedPackageDeployments",
		// sweepConstructReuse (epic memql#5414) reads before it knows whose,
		// for mineProcedureCorpusAcrossAutomations' reason: a schedule carries
		// no owner, so it lists owners and borrows each one's actor.
		"sweepConstructReuse",
		"sweepWaitingWorkRuns",
		// Installation-wide catalog maintenance has no machine owner. Read its
		// owner-only opt-in, then hand off to a bounded run owned by that person.
		"weeklyModelCatalogResearch",
		"workJournalRetentionSweep",
		// workerAppSessionStaleSweep is the fourth worker sweep and the
		// promise v1:worker:appSession's own status description made before
		// it existed. A session is held by one replica and only that replica
		// ends its row, so a pod restart left it at `running` for good --
		// visible, and counted against the owner's session cap. Its read
		// spans owners for the reason the sweeps below do, and without the
		// principal it returns zero rows and no error.
		"workerAppSessionStaleSweep",
		"workerInvocationRetentionSweep",
		// workerModelProbeStaleSweep (epic memql#5146) is the entry below's
		// twin: same claim shape, same silence when the claiming replica
		// dies, same zero-rows-and-no-error without the principal. What is
		// left on somebody's screen differs only in the noun -- a probe that
		// will never finish rather than a pull that will never move.
		"workerModelProbeStaleSweep",
		// workerModelPullStaleSweep (epic memql#5103) was the first entry
		// here that is not a retention sweep. It closes
		// model pulls whose claiming agent replica has gone: a pull is
		// claimed by exactly one replica, which is what makes the claim
		// lock-free, and nothing raises an event when the process holding it
		// dies. Its read spans owners because a sweep for abandoned work
		// cannot know whose work was abandoned before it looks, and without
		// the principal it returns zero rows and no error -- leaving a
		// progress bar on somebody's screen that will never move again.
		"workerModelPullStaleSweep",
		// workerStaleConnectionSweep (epic memql#5327, design D7) is the
		// third worker sweep and the one whose absence was a LIE rather than
		// a stall. The two above close rows somebody is WATCHING, so their
		// cost is a spinner; this one corrects a row somebody is BELIEVING,
		// and the belief is "that machine is up". connectedNodeId names the
		// replica holding a stream and is cleared only by a graceful close,
		// which a SIGKILLed pod does not perform and every redeploy causes.
		// Its read spans owners for the reason theirs do -- a sweep for
		// stranded holds cannot know whose machine was stranded before it
		// looks -- and without the principal it returns zero rows and no
		// error on a cluster where every stamp is dead.
		"workerStaleConnectionSweep",
	}
	got := auth.MaintenanceAutomationNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the maintenance list is %v, pinned as %v.\n\n"+
			"An addition is the most privileged change available to an automation: it runs as a "+
			"synthetic CLUSTER OWNER, past every owned-tier @rowAuthz in the tree. Two properties an "+
			"entry must have (component/auth/maintenance_actor.go states them in full):\n"+
			"  - the automation is ENGINE-OWNED, living in this repo's dsl/ tree;\n"+
			"  - its reads span owners BY NATURE -- a sweep, a cross-owner roll-up -- not merely\n"+
			"    finding an owner-scoped read inconvenient. For that, borrow ONE owner's authority\n"+
			"    with auth.ContextWithUserActor at the call site instead.\n\n"+
			"If the change is right, update this pin and say why in the commit.", got, want)
	}

	// (2) Reasons.
	for _, name := range got {
		if strings.TrimSpace(auth.MaintenanceAutomationReason(name)) == "" {
			t.Errorf("%q is on the maintenance list with no reason recorded", name)
		}
	}

	// (3) Engine-owned: the name resolves in the embedded tree.
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	// Each name, with the automation the engine's own tree loads under it. A
	// name is the engine's only when the automation that carries it was read
	// from the engine's own layer of the tree (Automation.Source).
	byName := map[string][]*Automation{}
	for _, a := range loaded {
		if a != nil {
			byName[a.Name] = append(byName[a.Name], a)
		}
	}
	// REACHABLE POSITIVE: without this, an empty load makes every assertion
	// below vacuous and the gate reports success over nothing.
	if len(byName) == 0 {
		t.Fatal("the embedded tree yielded no automations, so the membership checks below would pass over nothing")
	}
	for _, name := range got {
		copies := byName[name]
		if len(copies) == 0 {
			t.Errorf("%q is on the maintenance list but no automation of that name loads from this repo's "+
				"dsl/ tree (%d loaded). A grant that names nothing is a grant waiting for whoever defines "+
				"the name.", name, len(byName))
			continue
		}
		for _, a := range copies {
			if a.Source != memqldsl.SourceEngine {
				t.Errorf("%q loads from %s (source %s), which is not the engine's own tree: a listed name "+
					"belongs to the engine's automation alone", name, a.Origin, a.Source)
			}
		}
	}

	// (4) The join, in both directions -- and only from the engine's tree. A
	// listed name is granted to the automation the engine's tree loads under it,
	// and to that name from no other origin: asked with any other origin, the
	// list answers nil, so it cannot be satisfied by a pack, a runtime domain
	// or anything else carrying the name.
	for _, name := range got {
		for _, a := range byName[name] {
			ac, ok := auth.AccessFromContext(contextWithSystemActor(context.Background(), a))
			if !ok || !ac.IsClusterOwner() {
				t.Errorf("contextWithSystemActor(%s) is not a cluster owner, so the grant is declared and not "+
					"wired -- and every read it exists to enable returns zero rows, silently", a.Origin)
			}
		}
		if auth.MaintenanceActor(auth.AutomationOriginOther, name) != nil || auth.IsMaintenanceAutomation(auth.AutomationOriginOther, name) {
			t.Errorf("%q is granted to an origin that is not the engine's own tree", name)
		}
		for _, source := range []memqldsl.Source{memqldsl.SourceUnknown, memqldsl.SourcePack, memqldsl.SourceRuntime} {
			ac, ok := auth.AccessFromContext(contextWithSystemActor(context.Background(), &Automation{Name: name, Source: source}))
			if !ok || ac.IsClusterOwner() || ac.UserId != systemActorPrefix+name || ac.Role != auth.RoleReader {
				t.Errorf("an automation named %q from source %s runs as %+v, want the ordinary system actor", name, source, ac)
			}
		}
	}
	unlisted := "anAutomationNobodyGranted"
	if auth.IsMaintenanceAutomation(auth.AutomationOriginEngine, unlisted) {
		t.Fatalf("%q reads as listed; pick a name that is not", unlisted)
	}
	ac, ok := auth.AccessFromContext(contextWithSystemActor(context.Background(), &Automation{Name: unlisted, Source: memqldsl.SourceEngine}))
	if !ok {
		t.Fatal("contextWithSystemActor stamped no AccessContext at all")
	}
	if ac.IsClusterOwner() {
		t.Errorf("contextWithSystemActor(%q) is a CLUSTER OWNER and that name is on no list. Every "+
			"automation in the tree would hold the grant, which is the blanket elevation the named "+
			"principal exists instead of.", unlisted)
	}
	if ac.Role != auth.RoleReader {
		t.Errorf("the ordinary system actor carries role %q, want %q -- memql#2801's guarantee for a "+
			"caller with no identity of its own", ac.Role, auth.RoleReader)
	}
}

func TestRepositoryFeedNamesDoNotElevateInheritedCallers(t *testing.T) {
	for _, name := range []string{"pollPackageUpstreams", "notePackageUpstreamFromWebhook"} {
		for _, role := range []auth.Role{auth.RoleReader, auth.RoleDeveloper} {
			t.Run(name+"/"+string(role), func(t *testing.T) {
				caller := &auth.AccessContext{UserId: "feed-caller", Role: role}
				ctx := auth.ContextWithAccess(context.Background(), caller)
				got, ok := auth.AccessFromContext(contextWithSystemActor(ctx, &Automation{Name: name, Source: memqldsl.SourceEngine}))
				if !ok || got != caller || got.IsClusterOwner() {
					t.Fatalf("an inherited caller was replaced or elevated: %+v", got)
				}
			})
		}
	}
}

// TestMaintenanceActorIsNotMintableFromARequest pins the property that makes an
// identity safer than an enforcement bypass: the elevation comes from the
// compiled-in list, so nothing a caller supplies can reach it.
func TestMaintenanceActorIsNotMintableFromARequest(t *testing.T) {
	for _, name := range []string{
		"",
		"   ",
		"workerInvocationRetentionSweep ",    // trailing space: trimmed, so this one IS granted
		"workerInvocationRetentionSweepEvil", // prefix of a listed name
		"xworkerInvocationRetentionSweep",    // suffix of a listed name
		"system:maintenance:anything",        // the stamp's own prefix, asked for by name
		"WORKERINVOCATIONRETENTIONSWEEP",     // case
	} {
		ac := auth.MaintenanceActor(auth.AutomationOriginEngine, name)
		granted := ac != nil
		wantGranted := strings.TrimSpace(name) == "workerInvocationRetentionSweep"
		if granted != wantGranted {
			t.Errorf("MaintenanceActor(%q) granted=%v, want %v", name, granted, wantGranted)
		}
	}
	// Trimming is the ONE normalisation, and it is deliberate: a name arrives
	// from an automation's own declaration, where trailing whitespace is a
	// formatting artefact rather than a different automation. Case folding is
	// NOT applied, because two automations differing only in case would be two
	// automations.
	sorted := auth.MaintenanceAutomationNames()
	if !sort.StringsAreSorted(sorted) {
		t.Errorf("MaintenanceAutomationNames is not sorted: %v", sorted)
	}
}

// TestTheMaintenanceListIsNeverAskedWithANameAlone is the source gate on the
// rule above: the maintenance principal follows an automation's ORIGIN, so
// nothing in the tree may ask the list with a name alone -- nor with an origin
// it did not read from the automation.
//
// Three things are scanned, and each closes a way the rule could come back
// with every behavioural test still green:
//
//  1. Every call, anywhere in the repository, that asks with an origin
//     (maintenanceAsks) states the origin truthfully (maintenanceAskOffence):
//     read from the loaded automation's own source through
//     maintenanceOrigin(a), or the engine's origin stated as a constant for a
//     name fixed at compile time -- the engine's own Go acting as one of its
//     automations. A constant engine origin with a name that is not a
//     constant, a conversion of anything to the origin type, and any other
//     expression are offences.
//  2. In component/auth, the list itself is read only by the functions that
//     either take an origin or do not grant: a new reader of the map is a new
//     way to be granted, and it has to be argued here.
//  3. The executor's contextWithSystemActor takes the automation, not its
//     name: the origin can only come from the automation.
func TestTheMaintenanceListIsNeverAskedWithANameAlone(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	calls := 0
	consts := map[string]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !mentionsMaintenanceAsk(string(body)) {
			return nil
		}
		dir := filepath.Dir(path)
		if consts[dir] == nil {
			consts[dir] = packageStringConsts(t, dir)
		}
		found, offences := maintenanceAskOffences(path, body, consts[dir])
		calls += found
		offenders = append(offenders, offences...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// REACHABLE POSITIVE: the tree asks the list from dozens of places, so a
	// scan that finds a handful has stopped looking.
	if calls < 20 {
		t.Fatalf("found only %d calls to the maintenance list, expected 20+ -- the scan is broken, not the tree", calls)
	}
	if len(offenders) > 0 {
		t.Errorf("the maintenance list is asked with an origin that is not read from the automation:\n  %s",
			strings.Join(offenders, "\n  "))
	}

	// (2) The readers of the list in component/auth.
	readers := map[string]bool{
		"IsMaintenanceAutomation":     true, // takes the origin; MaintenanceActor goes through it
		"IsMaintenancePrincipal":      true, // answers from a principal the executor granted by origin
		"maintenancePrincipalOf":      true, // the same answer, read off the mark only MaintenanceActor sets
		"MaintenanceAutomationNames":  true, // names, for gates and messages; grants nothing
		"MaintenanceAutomationReason": true, // a reason string; grants nothing
	}
	authDir := filepath.Join(root, "component", "auth")
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read %s: %v", authDir, err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(authDir, e.Name())
		fset := token.NewFileSet()
		file, parseErr := goparser.ParseFile(fset, path, nil, goparser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			reads := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "maintenanceAutomations" {
					reads = true
				}
				return true
			})
			if !reads {
				continue
			}
			found[fn.Name.Name] = true
			if !readers[fn.Name.Name] {
				t.Errorf("%s:%d %s reads the maintenance list; a reader that is not argued in this gate is a way to be granted that nobody reviewed",
					path, fset.Position(fn.Pos()).Line, fn.Name.Name)
			}
			if fn.Name.Name == "IsMaintenanceAutomation" && !firstParamIs(fn, "AutomationOrigin") {
				t.Errorf("IsMaintenanceAutomation does not take an AutomationOrigin first")
			}
		}
	}
	if !found["IsMaintenanceAutomation"] {
		t.Fatal("found no reader of the maintenance list in component/auth -- the scan is broken, not the tree")
	}

	// (3) contextWithSystemActor takes the automation.
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, "executor.go", nil, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	seen := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "contextWithSystemActor" {
			continue
		}
		seen = true
		params := fn.Type.Params.List
		star, ok := params[len(params)-1].Type.(*ast.StarExpr)
		if id, isIdent := star.X.(*ast.Ident); !ok || !isIdent || id.Name != "Automation" {
			t.Errorf("contextWithSystemActor must take the *Automation, whose recorded origin decides the principal, not its name")
		}
	}
	if !seen {
		t.Fatal("contextWithSystemActor is not in executor.go -- move this check with it")
	}
}

// maintenanceAsks are the functions asked with an automation's origin first.
var maintenanceAsks = map[string]bool{"MaintenanceActor": true, "IsMaintenanceAutomation": true, "AutomationActor": true}

func mentionsMaintenanceAsk(body string) bool {
	for name := range maintenanceAsks {
		if strings.Contains(body, name+"(") {
			return true
		}
	}
	return false
}

// maintenanceAskOffences parses one Go file and answers how many asks it makes
// and which of them offend (maintenanceAskOffence). consts are the package's
// top-level string constants.
func maintenanceAskOffences(path string, body []byte, consts map[string]bool) (int, []string) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, body, goparser.SkipObjectResolution)
	if err != nil {
		return 0, []string{fmt.Sprintf("%s: does not parse: %v", path, err)}
	}
	calls := 0
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		if !maintenanceAsks[name] {
			return true
		}
		calls++
		if why := maintenanceAskOffence(path, call, consts); why != "" {
			out = append(out, fmt.Sprintf("%s:%d %s: %s", path, fset.Position(call.Pos()).Line, name, why))
		}
		return true
	})
	return calls, out
}

// maintenanceAskOffence says what is wrong with one ask, or "".
func maintenanceAskOffence(path string, call *ast.CallExpr, consts map[string]bool) string {
	if len(call.Args) != 2 {
		return "does not pass an origin and a name"
	}
	origin, name := call.Args[0], call.Args[1]
	// MaintenanceActor hands IsMaintenanceAutomation the origin it was itself
	// given.
	if id, ok := origin.(*ast.Ident); ok && id.Name == "origin" &&
		filepath.ToSlash(filepath.Clean(path)) == "../../component/auth/maintenance_actor.go" {
		return ""
	}
	// A test builds principals on purpose, to see what each handler does
	// with them; the rule is the production code's. A test may state either
	// constant origin for any name, and the zero origin as a literal.
	test := strings.HasSuffix(path, "_test.go")
	switch x := origin.(type) {
	case *ast.CallExpr:
		if fn, ok := x.Fun.(*ast.Ident); ok && fn.Name == "maintenanceOrigin" && len(x.Args) == 1 {
			return "" // read from the loaded automation's own source
		}
		if lit, ok := singleArg(x).(*ast.BasicLit); test && ok && lit.Kind == token.INT {
			return ""
		}
		return "the origin is not read from the loaded automation (only maintenanceOrigin(a) reads it); a conversion or a call is not an origin"
	case *ast.SelectorExpr, *ast.Ident:
		constant := ""
		if sel, ok := x.(*ast.SelectorExpr); ok {
			constant = sel.Sel.Name
		} else {
			constant = x.(*ast.Ident).Name
		}
		switch constant {
		case "AutomationOriginOther":
			return "" // grants nothing, whatever the name
		case "AutomationOriginEngine":
			if test || isCompileTimeName(name, consts) {
				return "" // the engine's own Go acting as one of its automations
			}
			return "the engine's origin is stated as a constant for a name that is not one: a name read at run time must come with the origin read from its automation, maintenanceOrigin(a)"
		}
	}
	return "the origin is neither maintenanceOrigin(a) nor an AutomationOrigin constant"
}

func singleArg(call *ast.CallExpr) ast.Expr {
	if len(call.Args) != 1 {
		return nil
	}
	return call.Args[0]
}

// isCompileTimeName reports whether e is a string fixed at compile time: a
// literal, one of the package's string constants, or a sum of them.
func isCompileTimeName(e ast.Expr, consts map[string]bool) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.Ident:
		return consts[x.Name]
	case *ast.ParenExpr:
		return isCompileTimeName(x.X, consts)
	case *ast.BinaryExpr:
		return x.Op == token.ADD && isCompileTimeName(x.X, consts) && isCompileTimeName(x.Y, consts)
	}
	return false
}

// packageStringConsts collects the names of the top-level constants declared
// in dir's Go files.
func packageStringConsts(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		file, err := goparser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, goparser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, n := range spec.(*ast.ValueSpec).Names {
					out[n.Name] = true
				}
			}
		}
	}
	return out
}

// TestTheSourceGateRefusesAnOriginNotReadFromTheAutomation shows the gate
// failing on each form an untrue origin takes, and passing the true ones.
func TestTheSourceGateRefusesAnOriginNotReadFromTheAutomation(t *testing.T) {
	consts := map[string]bool{"listedSweep": true}
	for src, wantOffence := range map[string]bool{
		`auth.MaintenanceActor(auth.AutomationOriginEngine, a.Name)`:                 true,
		`auth.MaintenanceActor(auth.AutomationOrigin(1), "sweep")`:                   true,
		`auth.MaintenanceActor(auth.AutomationOrigin(a.Source), a.Name)`:             true,
		`auth.MaintenanceActor(originOf(a), a.Name)`:                                 true,
		`auth.MaintenanceActor(origin, a.Name)`:                                      true,
		`auth.IsMaintenanceAutomation(auth.AutomationOriginEngine, name)`:            true,
		`auth.AutomationActor(auth.AutomationOriginEngine, a.Name)`:                  true,
		`auth.MaintenanceActor(auth.AutomationOriginEngine)`:                         true,
		`auth.MaintenanceActor(maintenanceOrigin(a), a.Name)`:                        false,
		`auth.MaintenanceActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns")`: false,
		`auth.MaintenanceActor(auth.AutomationOriginEngine, listedSweep)`:            false,
		`auth.MaintenanceActor(auth.AutomationOriginOther, a.Name)`:                  false,
	} {
		body := []byte("package probe\n\nfunc probe() { _ = " + src + " }\n")
		n, offences := maintenanceAskOffences("probe.go", body, consts)
		if n != 1 {
			t.Fatalf("%s: the scan found %d asks, want 1", src, n)
		}
		if got := len(offences) > 0; got != wantOffence {
			t.Errorf("%s: offence=%v (%v), want %v", src, got, offences, wantOffence)
		}
	}
}

func firstParamIs(fn *ast.FuncDecl, typeName string) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}
	id, ok := fn.Type.Params.List[0].Type.(*ast.Ident)
	return ok && id.Name == typeName
}
