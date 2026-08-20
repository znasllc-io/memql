// Package fleet holds the gates for the MemQL Cloud control-plane DSL bundle
// (epic memql#3852, task memql#3853).
//
// # Why this file exists at all
//
// The fleet DSL is a PRODUCT bundle. It is not embedded (`dsl/embed.go` does
// not name it), it is not compiled into any binary, and it reaches a node at
// runtime through MEMQL_DSL_PATH -- which is exactly the delivery any
// customer's product uses, and exactly why MemQL Cloud can run on a
// product-neutral engine.
//
// The cost of that is that NOTHING ELSE IN THIS REPOSITORY LOOKS AT IT.
// test/dslconformance walks `dsl.Tree()`, which is the embedded tree plus
// plugin-registered subtrees; a directory under deploy/ is in neither. So a
// bundle that stopped parsing, or that referenced a mutation somebody renamed
// in the engine tree, would be discovered by a customer's node refusing to boot.
//
// This file closes that. It runs the SAME pipeline `cmd/memqllint` runs -- the
// import-graph load, the referential-integrity passes, and the engine-parity
// pass that mounts this tree as an overlay on the embedded core tree and runs
// MemQLEngine.Init over the merged result. A bundle that passes those MOUNTS at
// boot.
//
// # What that pipeline does NOT catch, measured rather than assumed
//
// Mounting is not the same as working, and the difference was found the way
// these things have to be found -- by breaking something on purpose and
// watching the gate stay green. Renaming a query's `shape` clause to a shape
// that does not exist passes `memqllint` and passes the parity pass: the tree
// loads, the query registers, and the failure arrives the first time somebody
// CALLS it. The unused-import lane does not save you either, because the
// import stays "used" as long as one other construct in the file still
// references that shape.
//
// TestEveryQueryShapeIsDefined below covers that one specific gap. It is not a
// general resolver and does not pretend to be; it exists because that is the
// class this bundle actually hit, and because a gate whose limits are unstated
// gets read as proving more than it does.
package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

func bundleRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(self), "dsl")
}

// TestFleetBundleLoadsAndMounts is the gate. It is deliberately the same three
// passes `memqllint <dir>` runs, in the same order, because "lints clean" and
// "boots" have to be the same statement for a tree nothing else validates.
func TestFleetBundleLoadsAndMounts(t *testing.T) {
	root := os.DirFS(bundleRoot(t))

	tree, err := dslimports.Load(root)
	if err != nil {
		t.Fatalf("the fleet bundle does not load:\n%v", err)
	}
	if tree == nil {
		t.Fatal("dslimports.Load returned no tree and no error")
	}

	var problems []string
	for _, e := range tree.VerifyReferentialIntegrity() {
		problems = append(problems, "referential integrity: "+e.Error())
	}
	for _, e := range tree.VerifyAllSymbolReferences() {
		problems = append(problems, "symbol reference: "+e.Error())
	}
	for _, e := range tree.VerifyPreambleAttachment() {
		problems = append(problems, "preamble attachment: "+e.Error())
	}

	// The engine-parity pass. dslimports models parse + import-graph integrity;
	// the engine runs a SECOND validation tier at boot that a tree passing
	// dslimports can still trip -- non-canonical @relationship types,
	// declared-but-unused mutation args, CQS violations, per-kind uniqueness.
	// Mounting this tree over the embedded core tree and running Init is what
	// makes a clean result here mean "this boots".
	diags, skipped, perr := memql.LintUnifiedTree(nil, root)
	if perr != nil {
		problems = append(problems, "engine-parity lint: "+perr.Error())
	}
	for _, d := range diags {
		if d.File != "" {
			problems = append(problems, d.File+": "+d.Message)
		} else {
			problems = append(problems, d.Message)
		}
	}

	// A domain skipped by the parity pass was NOT parity-checked, and a clean
	// report without this check is ambiguous between "checked and clean" and
	// "never checked". `fleet` is a new namespace and must never collide with a
	// core embedded domain; if it ever does, the embedded tree wins and this
	// bundle silently stops being loaded at all.
	for _, d := range skipped {
		problems = append(problems, "domain "+d+" was SKIPPED by the engine-parity pass -- it collides with a core embedded domain, so the embedded tree owns the namespace and this bundle's version of it is never loaded")
	}

	if len(problems) > 0 {
		t.Fatalf("the fleet bundle would not mount cleanly:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestEveryQueryShapeIsDefined closes the gap the package comment measures.
//
// A `shape <name>` clause naming a shape that does not exist survives the whole
// load pipeline: the bundle mounts, the query registers, and the first caller
// gets the error. That is the worst place for it -- a customer's console, at
// runtime, on a read that has always worked.
func TestEveryQueryShapeIsDefined(t *testing.T) {
	dir := filepath.Join(bundleRoot(t), "fleet")

	shapesSrc, err := os.ReadFile(filepath.Join(dir, "shapes.memql"))
	if err != nil {
		t.Fatalf("read shapes.memql: %v", err)
	}
	defined := map[string]bool{}
	// `shape <Concept> <name> {` -- the declaration form.
	for _, m := range regexp.MustCompile(`(?m)^shape\s+\w+\s+(\w+)\s*\{`).FindAllStringSubmatch(string(shapesSrc), -1) {
		defined[m[1]] = true
	}
	if len(defined) == 0 {
		t.Fatal("shapes.memql declares no shapes -- either the file emptied out or this parse stopped matching, and either way this gate is watching nothing")
	}

	queriesSrc, err := os.ReadFile(filepath.Join(dir, "queries.memql"))
	if err != nil {
		t.Fatalf("read queries.memql: %v", err)
	}
	// `  shape   <name>` -- the reference form inside a query body. Anchored to
	// the start of an indented line so the word "shape" in prose cannot match.
	refs := regexp.MustCompile(`(?m)^\s+shape\s+(\w+)\s*$`).FindAllStringSubmatch(string(queriesSrc), -1)
	if len(refs) == 0 {
		t.Fatal("no query names a shape -- this gate is watching nothing")
	}
	for _, m := range refs {
		if !defined[m[1]] {
			t.Errorf("a query binds shape %q, which shapes.memql does not declare. This mounts cleanly and fails on the first call.", m[1])
		}
	}
}

// TestFleetBundleIsNotEmbedded is the product-neutrality half.
//
// The engine is product-neutral by doctrine, and MemQL Cloud is a product. The
// moment this bundle is named in `dsl/embed.go` it is compiled into every
// engine binary -- including the ones a customer runs -- and the acceptance bar
// the consolidation epic sets ("a second product boots a full stack with ZERO
// engine-repo edits") stops being something this repository can demonstrate,
// because its own product would no longer be meeting it.
//
// The two structural guards that already exist do not cover this:
// TestEngineIsProductNeutral is a banned-NAMES list, and `fleet` is not a name
// anybody thought to ban; TestClientsDirectoryIsAllowlisted polices `clients/`
// and this is under `deploy/`.
func TestFleetBundleIsNotEmbedded(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(self)))

	b, err := os.ReadFile(filepath.Join(repo, "dsl", "embed.go"))
	if err != nil {
		t.Fatalf("read dsl/embed.go: %v", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if !strings.Contains(line, "go:embed") {
			continue
		}
		for tok := range strings.FieldsSeq(line) {
			if tok == "all:fleet" || tok == "fleet" {
				t.Fatalf("dsl/embed.go embeds the `fleet` domain (%q). The MemQL Cloud control plane is a PRODUCT and must reach a node through MEMQL_DSL_PATH; compiling it into the engine puts our own product inside the product-neutral engine every customer runs.", strings.TrimSpace(line))
			}
		}
	}
}

// TestFleetBundleShipsNoGo. A DSL bundle is DATA -- an init-container copies the
// tree into a volume the node reads, and the image has no compiler in it. A .go
// file here would not be a compile error; it would be a file that is silently
// never built, whose absence from the running system is discovered when
// somebody wonders why the thing it implements does not work.
//
// The exception is this file's own directory, which holds the gates rather than
// the bundle: the check is scoped to deploy/fleet/dsl/.
func TestFleetBundleShipsNoGo(t *testing.T) {
	err := filepath.Walk(bundleRoot(t), func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			t.Errorf("%s is a Go file inside the DSL bundle. The bundle image is data-only -- nothing compiles this, so it would ship as an inert file.", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the bundle: %v", err)
	}
}

// TestBundleImageCopiesTheBundle.
//
// The bundle image's entire content is one COPY line. Move or rename the DSL
// tree without updating it and `docker build` still succeeds -- COPY of a
// missing source fails, but COPY of the WRONG existing source does not, and
// neither does a tree that has simply moved out from under a path that still
// resolves to something. What ships is an image whose init-container copies
// nothing useful into the volume, so every node mounts an empty MEMQL_DSL_PATH
// and the control plane is silently absent: no fleet concepts, no automations,
// and a mesh that boots perfectly.
//
// This also ROUTES the Dockerfile through CI. `deploy/fleet/Dockerfile` matches
// no path-filter bucket on its own, and scripts/ci's coverage gate offers two
// remedies for that: exempt the file, or make a gate read it. Reading it is the
// better answer here precisely because the failure above is invisible.
func TestBundleImageCopiesTheBundle(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Dir(self)

	b, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	src := string(b)

	// The COPY's source is relative to the build context, which the Dockerfile's
	// own header documents as deploy/fleet. So the source has to be `dsl/`, and
	// that directory has to be the one the rest of this file gates.
	copyLine := regexp.MustCompile(`(?m)^COPY\s+(\S+)\s+(\S+)\s*$`).FindStringSubmatch(src)
	if copyLine == nil {
		t.Fatal("the bundle Dockerfile has no COPY line -- it would build an image containing no DSL at all, which mounts as an empty MEMQL_DSL_PATH and boots a mesh with no control plane")
	}
	if got := strings.TrimSuffix(copyLine[1], "/"); got != "dsl" {
		t.Errorf("the Dockerfile copies %q, but the bundle tree is deploy/fleet/dsl (the build context is deploy/fleet)", copyLine[1])
	}
	if _, serr := os.Stat(filepath.Join(dir, "dsl")); serr != nil {
		t.Errorf("the Dockerfile's COPY source does not exist: %v", serr)
	}

	// The destination is the contract with the dsl-bundle component, whose
	// init-container runs `cp -a /bundle/. /var/lib/memql/dsl/`. A different
	// destination here copies nothing and reports success.
	if dest := strings.TrimSuffix(copyLine[2], "/"); dest != "/bundle" {
		t.Errorf("the Dockerfile copies into %q; the dsl-bundle component's init-container reads /bundle, so anything else copies nothing and exits 0", copyLine[2])
	}
}

// TestFleetActionsResolveToAllowlistedScripts.
//
// A `capability script(script: "<id>")` call is only reachable when <id> is in
// component/automations/steps.capabilityScriptAllowlist -- that map is the
// security boundary, and the runner rejects an unregistered id BEFORE exec. The
// failure mode this closes is specific and quiet: an unregistered capability
// still runs fine from a human shell, so the script is testable, demoable and
// entirely inert on the engine path, with nothing about the tenant that failed
// to provision to say why.
//
// The check is textual because the two live on opposite sides of the
// compiled/not-compiled line: the actions are data under deploy/, the allowlist
// is Go under component/. Neither can import the other, and a test that only
// read one of them would be watching nothing.
func TestFleetActionsResolveToAllowlistedScripts(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(self)))

	actions, err := os.ReadFile(filepath.Join(bundleRoot(t), "fleet", "actions.memql"))
	if err != nil {
		t.Fatalf("read actions.memql: %v", err)
	}
	allow, err := os.ReadFile(filepath.Join(repo, "component", "automations", "steps", "capability_script.go"))
	if err != nil {
		t.Fatalf("read capability_script.go: %v", err)
	}

	// `script:` followed by any run of spaces and a quoted id. Matched rather
	// than split on a fixed column so re-aligning the arguments -- which a
	// formatter does -- cannot disarm this.
	scriptArg := regexp.MustCompile(`script:\s*"([^"]+)"`)
	var ids []string
	for _, m := range scriptArg.FindAllStringSubmatch(string(actions), -1) {
		ids = append(ids, m[1])
	}
	if len(ids) == 0 {
		t.Fatal("found no capability-script ids in actions.memql -- either the actions stopped naming one or this parse stopped matching, and either way this gate is watching nothing")
	}

	for _, id := range ids {
		if !strings.Contains(string(allow), `"`+id+`"`) {
			t.Errorf("action capability %q is not in capabilityScriptAllowlist -- the runner rejects it before exec, so the capability is silently inert on the engine path while running fine from a shell", id)
		}
	}
}

// TestFleetCapabilityScriptsExist closes the other half of the same pair: an
// allowlist entry pointing at a path that is not there. That one fails at exec
// time rather than at load, which is later and further from whoever caused it.
func TestFleetCapabilityScriptsExist(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(self)))

	b, err := os.ReadFile(filepath.Join(repo, "component", "automations", "steps", "capability_script.go"))
	if err != nil {
		t.Fatalf("read capability_script.go: %v", err)
	}
	entry := regexp.MustCompile(`"fleet\.[A-Za-z]+":\s*"(scripts/fleet/[^"]+)"`)
	matches := entry.FindAllStringSubmatch(string(b), -1)
	for _, m := range matches {
		if _, serr := os.Stat(filepath.Join(repo, m[1])); serr != nil {
			t.Errorf("capabilityScriptAllowlist points at %s, which does not exist: %v", m[1], serr)
		}
	}
	if len(matches) == 0 {
		t.Fatal("no fleet.* entries found in capabilityScriptAllowlist")
	}
}
