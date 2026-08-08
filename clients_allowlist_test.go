package main

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// allowedClients is the closed set of directories permitted under `clients/`,
// each paired with why it is a PLATFORM surface rather than a product.
//
// One entry today, and that is the point: the list is meant to be short and
// arguing to extend it is meant to be the moment someone re-reads the boundary
// below. Adding a row is a deliberate, reviewable act; landing a directory is
// otherwise the convenient path.
//
// DELIBERATELY NOT CONFIGURABLE FROM INSIDE clients/. A marker file or a
// manifest that the policed tree supplies is a gate the policed change can open
// for itself -- the same self-disabling shape this file's placement (see below)
// exists to avoid.
var allowedClients = map[string]string{
	"portal": "the platform's own graphical operations console; served by component/portal (memql#3314)",
}

// TestClientsDirectoryIsAllowlisted is the structural half of engine product
// neutrality (znasllc-io/memql#3326).
//
// # The boundary
//
// The engine repo hosts the PLATFORM's own client surfaces -- its ops console.
// A customer's SPA is a product, and a product's client belongs in a product
// repo built from the `memql-project` template. Platform consolidation's
// acceptance bar is "a second product boots a full stack with ZERO engine-repo
// edits" (docs/internal/design/platform-consolidation.md); that bar fails
// silently the moment products start living here.
//
// # Why the sibling guard is not enough
//
// TestEngineIsProductNeutral (product_neutrality_test.go) is a BANNED-NAMES
// list: it fails when one of the specific product names this repo shed appears
// in a tracked file. It cannot notice a NEW product SPA landing in
// `clients/acme-spa/` under a name nobody thought to ban. The two guards are
// complementary and neither subsumes the other -- one polices vocabulary
// repo-wide, this one polices structure in the one directory whose whole
// purpose is to host client applications.
//
// # WHY IT LIVES IN THE ROOT PACKAGE
//
// The lesson of scripts/dev/vscode_lane_scope_test.go: a guard placed where the
// thing it polices could disable it goes silent at exactly the wrong moment.
// Both dodges have to run this test.
//
//   - Adding `clients/acme-spa/` (TypeScript only, no `.go` file) sets the
//     `portal` bucket, NOT `go` -- and portal-checks runs npm, not Go. So
//     `clients/**` is also a `gates` entry in .github/workflows/ci.yml, which
//     makes RUN_GATES fire and the gate-inputs step run `go test ./`. The
//     `gates` bucket is precisely "a non-Go file that a Go gate reads"
//     (memql#2972), and this test reads the `clients/` tree, so the routing is
//     the bucket's stated purpose rather than a special case.
//   - Deleting an entry from allowedClients (or this file) edits a `.go` file,
//     which sets the `go` bucket and runs go-tests -- the lane that runs the
//     root package UNCACHED (`-count=1`), because these root gates sweep
//     `git ls-files` and Go's cache does not key on what a subprocess returned.
//   - Dropping `clients/**` from the `gates` bucket edits
//     .github/workflows/**, which sets the `ci` bucket and runs everything.
//     scripts/dev/gate_inputs_lane_scope_test.go additionally carries a row for
//     this gate, so that removal turns red rather than merely going quiet.
//
// A guard in scripts/dev instead would work for the same reasons; the root
// package is where every other `git ls-files`-sweeping gate in this repo lives,
// next to the product-neutrality guard it complements.
func TestClientsDirectoryIsAllowlisted(t *testing.T) {
	// Police GIT-TRACKED files only, the way product_neutrality_test.go does:
	// an untracked scratch directory under clients/ is not repo content, and a
	// tracked one cannot hide behind .gitignore.
	out, err := exec.Command("git", "ls-files", "-z", "clients").Output()
	if err != nil {
		t.Fatalf("git ls-files clients: %v", err)
	}

	// name -> the tracked files that put it there, for the failure message.
	found := map[string][]string{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		parts := strings.Split(rel, "/")
		// parts[0] is "clients". A two-part path is a FILE directly under
		// clients/ (clients/README.md) -- the convention doc, not an
		// inhabitant. Only a third segment means a directory.
		if len(parts) < 3 || parts[0] != "clients" {
			continue
		}
		found[parts[1]] = append(found[parts[1]], rel)
	}

	// Anti-vacuous floor. A guard that enumerated nothing must fail rather than
	// report that every inhabitant is allowed, having checked none: a renamed
	// tree or a broken `git ls-files` would otherwise turn this green forever.
	if len(found) == 0 {
		t.Fatal("found no tracked directories under clients/, but the repo carries " +
			"the portal. This guard cannot pass vacuously: if clients/ moved, retarget " +
			"it rather than deleting it (memql#3326)")
	}

	allowed := make([]string, 0, len(allowedClients))
	for name := range allowedClients {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, ok := allowedClients[name]; ok {
			continue
		}
		files := found[name]
		sort.Strings(files)
		if len(files) > 5 {
			files = append(files[:5:5], "...")
		}
		t.Errorf("clients/%s/ is not in the clients/ allowlist.\n\n"+
			"THE BOUNDARY: the engine repo hosts the PLATFORM's own client surfaces "+
			"-- its ops console. A customer's SPA is a product, and a product's client "+
			"belongs in a product repo built from the `memql-project` template, not "+
			"here. Platform consolidation's acceptance bar is that a second product "+
			"boots a full stack with ZERO engine-repo edits "+
			"(docs/internal/design/platform-consolidation.md); products living in "+
			"clients/ fail that bar silently.\n\n"+
			"IF %s IS A PRODUCT'S CLIENT: move it to that product's own repo. Nothing "+
			"in the engine needs to change for it to work -- that is the whole design "+
			"(clients/README.md).\n"+
			"IF %s IS A PLATFORM SURFACE (an ops console, a demo the engine itself "+
			"serves, a landing page for memQL): add it to allowedClients in "+
			"clients_allowlist_test.go with a one-line reason, in the same PR.\n\n"+
			"allowed today: %v\ntracked files that put it here: %v",
			name, name, name, allowed, files)
	}

	// A stale entry pre-authorizes a name nobody is watching: whoever next adds
	// `clients/<that name>/` gets waved through by a row that outlived the
	// client it described.
	for _, name := range allowed {
		if _, ok := found[name]; !ok {
			t.Errorf("allowedClients names %q, but clients/%s/ has no tracked files. "+
				"Remove the row -- a stale entry silently pre-authorizes that name for "+
				"whatever lands under it next (memql#3326).", name, name)
		}
	}
}
