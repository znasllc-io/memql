package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// db_operand_image_test.go -- znasllc-io/memql#4458, defect 2.
//
// THE INVARIANT. `memql-db:16-dev` is the database operand image the LOCAL
// overlay names, and it exists in no registry -- it is a `make db-image`
// artifact. So a k3d cluster running that overlay can only get it one way:
// something builds it and imports it. Unlike every other image in the local
// tree, k3s cannot fall back to pulling it.
//
// `scripts/k3d/dev.sh` is that something, and its call site says so in as many
// words ("UNCONDITIONAL, unlike the infra images above ... a cluster without it
// cannot fall back to pulling"). What this test defends is that the FUNCTION
// keeps the promise the call site makes.
//
// HOW IT WAS BROKEN. `ensure_db_image` opened with an early return on
// `--image-source=checkout`, reasoning that the lane "leaves the operand
// override in place (the database is not a node)". Both clauses are true and
// the conclusion inverts: the override being left alone is exactly WHY the
// image has to exist locally, because what the overlay then names is
// `memql-db:16-dev`.
//
// WHY NOBODY SAW IT. `make up` reaches dev.sh through bringup.sh WITHOUT the
// flag, so every developer machine built the image and the skip never fired
// there. The only caller that passes `--image-source=checkout` is the install
// wizard's from-source lane (`install-main.json`'s buildImages step) -- and on
// a machine whose earlier installs used release images, nothing had ever built
// it. The CNPG pod sat in ImagePullBackOff for the whole run, every engine node
// crashlooped behind it, and the single line about it in the log read as fine.
//
// The rule this encodes: THE SKIP IS "IS IT ALREADY THERE", NEVER "WHICH LANE
// IS THIS". A presence probe verifies its assumption; a flag asserts one.

func devScript(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("scripts/k3d/dev.sh")
	if err != nil {
		t.Fatalf("read scripts/k3d/dev.sh: %v", err)
	}
	return string(body)
}

// ensureDbImageBody returns the text of ensure_db_image, or fails.
func ensureDbImageBody(t *testing.T, script string) string {
	t.Helper()
	const open = "function ensure_db_image() {"
	start := strings.Index(script, open)
	if start < 0 {
		t.Fatal("scripts/k3d/dev.sh has no ensure_db_image -- if the operand image is " +
			"provisioned somewhere else now, point this test there rather than deleting it: " +
			"memql-db:16-dev exists in no registry and a cluster cannot pull it")
	}
	body := script[start+len(open):]
	// The function ends at the first line that closes it at column 0.
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	return body
}

func TestTheDatabaseOperandImageIsEnsuredByPresenceNotByLane(t *testing.T) {
	script := devScript(t)
	body := ensureDbImageBody(t, script)

	if strings.Contains(body, "IMAGE_SOURCE") {
		t.Errorf("ensure_db_image branches on IMAGE_SOURCE.\n\n"+
			"That is memql#4458 defect 2. `memql-db:16-dev` is named by the local overlay\n"+
			"and exists in no registry, so skipping it on a LANE -- rather than on whether\n"+
			"the image is already in the cluster -- leaves the CNPG pod in ImagePullBackOff\n"+
			"forever, with every engine node crashlooping behind it and nothing in the log\n"+
			"that reads as a failure.\n\n"+
			"The skip belongs on `cluster_holds_db_image`, which VERIFIES the assumption the\n"+
			"flag merely asserted.\n\nbody was:\n%s", body)
	}

	// The presence probe is what the skip must be keyed on. Asserting it is
	// present stops the fix being "delete the early return and always rebuild",
	// which would put a multi-minute image build in every `make dev`.
	if !strings.Contains(body, "cluster_holds_db_image") {
		t.Error("ensure_db_image no longer consults cluster_holds_db_image, so either it " +
			"rebuilds the operand image on every inner-loop `make dev` (~500MB, minutes), " +
			"or it has stopped ensuring anything at all")
	}
}

func TestTheDatabaseOperandCallSiteStaysUnconditional(t *testing.T) {
	// The other half: a correct function called inside an `if` is the same bug
	// one level out. The call sits in main() at column 4 with nothing but the
	// comment above it.
	script := devScript(t)
	idx := strings.Index(script, "\n    ensure_db_image\n")
	if idx < 0 {
		t.Fatal("ensure_db_image is no longer called unconditionally from dev.sh's main(); " +
			"a cluster without memql-db:16-dev cannot fall back to pulling it")
	}
}

// A staged capability script reaching OUTSIDE the staged tree, which is a
// missing-file error at run time on the one path nobody exercises locally.
//
// WHAT THE VSIX ACTUALLY SHIPS (scripts/vscode/package.sh): the capability
// scripts named in runner.ts's CAPABILITY_SCRIPTS, plus `scripts/lib/*.sh`,
// the install graph documents and the tool pins. Nothing else. So a staged
// script that resolves a sibling through `${SCRIPT_DIR}/../<dir>/` is fine for
// `lib` and is a run-time failure for anything else -- and it fails ONLY when
// the extension runs it, never for `make dev`, where SCRIPT_DIR and the repo
// root are the same tree.
//
// That is precisely how memql#4458's own fix nearly shipped broken: removing
// the lane skip from `ensure_db_image` made a `${SCRIPT_DIR}/../db-image/`
// line reachable from the wizard for the first time, and `scripts/db-image/`
// is not staged. The right root there is `${REPO_ROOT}`, which the wizard sets
// to the checkout it just cloned -- a full clone, which has everything.
//
// `source` lines are already covered at package time by package.sh's
// verify_staged_sources; this covers the INVOCATIONS, and it runs on every PR
// rather than only when someone packages a VSIX.
// stagedSet is exactly what scripts/vscode/package.sh puts in the VSIX: the
// capability scripts runner.ts declares, plus scripts/lib/*.sh, the install
// graph documents and the tool pins. Derived from the same two sources
// package.sh derives it from, so it cannot drift into agreeing by accident.
func stagedSet(t *testing.T) map[string]bool {
	t.Helper()
	runner, err := os.ReadFile(filepath.Join("editors", "vscode", "src", "install", "runner.ts"))
	if err != nil {
		t.Fatalf("read runner.ts: %v", err)
	}
	declared := regexp.MustCompile(`"[a-zA-Z0-9._]+":\s*"(scripts/[^"]+\.sh)"`)
	set := map[string]bool{}
	for _, m := range declared.FindAllStringSubmatch(string(runner), -1) {
		set[m[1]] = true
	}
	if len(set) == 0 {
		t.Fatal("parsed no capability scripts out of runner.ts -- the detector matches " +
			"nothing, so a pass here would mean nothing")
	}
	for _, pattern := range []string{
		filepath.Join("scripts", "lib", "*.sh"),
		filepath.Join("scripts", "install", "graph", "*.json"),
	} {
		matches, globErr := filepath.Glob(pattern)
		if globErr != nil || len(matches) == 0 {
			t.Fatalf("staged pattern %s matched nothing", pattern)
		}
		for _, m := range matches {
			set[filepath.ToSlash(m)] = true
		}
	}
	set["scripts/install/tool-pins.env"] = true
	return set
}

// A staged capability script reaching for a file the VSIX does not ship, which
// is a missing-file error at run time on the one path nobody exercises locally.
//
// WHAT THE VSIX SHIPS is `stagedSet` above, and nothing else. So a staged
// script that resolves a sibling through `${SCRIPT_DIR}/../<path>` is fine when
// that path is staged and is a run-time failure when it is not -- and it fails
// ONLY when the extension runs it, never for `make dev`, where SCRIPT_DIR and
// the repo root are the same tree.
//
// That is precisely how memql#4458's own fix nearly shipped broken: removing
// the lane skip from `ensure_db_image` made a `${SCRIPT_DIR}/../db-image/`
// line reachable from the wizard for the first time, and `scripts/db-image/` is
// not staged (nor should it be, at ~500MB of build context). The right root
// there is `${REPO_ROOT}`, which the wizard sets to the checkout it just
// cloned -- a full clone, which has everything.
//
// `source` lines are already covered at PACKAGE time by package.sh's
// verify_staged_sources; this covers the INVOCATIONS and the parameter
// defaults, and it runs on every PR rather than only when someone packages.
func TestStagedCapabilityScriptsDoNotReachOutsideTheStagedTree(t *testing.T) {
	staged := stagedSet(t)
	// `${SCRIPT_DIR}/../<sibling>/...` -- a SIBLING directory under scripts/,
	// which is the shape of the bug. The whole path is captured, not just the
	// directory, because scripts/install/ holds both staged capability scripts
	// and unstaged Go: a directory-level check would be wrong AND reassuring.
	//
	// THE ROOT CLIMB `${SCRIPT_DIR}/../..` IS DELIBERATELY NOT MATCHED, and
	// this is the exemption clause rather than an oversight. Climbing to the
	// repository root is the correct pattern -- it is how up.sh and dev.sh
	// compute their REPO_ROOT default before `--repo-root` overrides it, which
	// is exactly the fix this test recommends. The one guarded root-climb that
	// does read a repo file, bringup.sh's `../../deploy/k8s/base/migrate-job.yaml`,
	// tests for the file and warns its way past a miss into the identity
	// boot-migration fallback; `k3d.bringup` is also not in the install graph,
	// so the wizard never runs it. If a root climb ever reads a repo file
	// UNGUARDED, widen this pattern rather than exempting the call site.
	reach := regexp.MustCompile(`\$\{SCRIPT_DIR\}/\.\./([A-Za-z0-9_-][A-Za-z0-9_./-]*)`)
	checked := 0
	for rel := range staged {
		if !strings.HasSuffix(rel, ".sh") {
			continue
		}
		body, readErr := os.ReadFile(rel)
		if readErr != nil {
			continue // declared but absent is runner.ts's problem, not this test's
		}
		checked++
		for _, hit := range reach.FindAllStringSubmatch(string(body), -1) {
			target := "scripts/" + strings.TrimSuffix(hit[1], "/")
			if staged[target] {
				continue
			}
			// A bare directory reach (`${SCRIPT_DIR}/../lib/`) resolves to
			// whatever follows in the same expression; scripts/lib is wholly
			// staged, so treat a staged DIRECTORY as satisfied too.
			if strings.HasPrefix(target, "scripts/lib") {
				continue
			}
			t.Errorf("%s resolves `${SCRIPT_DIR}/../%s`, which the VSIX does not ship.\n"+
				"The staged set is the capability scripts, scripts/lib/*.sh, the graph\n"+
				"documents and the tool pins -- so this path does not exist when the\n"+
				"extension runs the script. It works for `make dev` (same tree) and fails\n"+
				"for every operator.\n"+
				"  fix: resolve it from ${REPO_ROOT} -- the checkout the run builds from --\n"+
				"       or add it to the staged set if it genuinely belongs in a VSIX.\n"+
				"  See memql#4458.", rel, hit[1])
		}
	}
	if checked == 0 {
		t.Fatal("read none of the declared capability scripts; the walk is not reaching them")
	}
}
