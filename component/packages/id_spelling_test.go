package packages

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// id_spelling_test.go -- a stored relationship field and a request id are
// two spellings of one value (memql#5291).
//
// `packageId` on v1:platform:packageDeployment is an outgoing relationship,
// so the engine canonicalizes it on write and an in-process read returns it
// canonical: `v1:platform:package:<id>`. The request carries the BARE id,
// because the engine bare-ifies every id on egress and a client never
// composes the canonical form (docs/public/concepts/identifiers.md) -- and a
// builtin's arguments are not canonicalized on the way in. The read path
// already resolves a bare id in SQL, so the compare is the ONLY place the two
// spellings meet, and it is the compare that goes through BareShortId on both
// sides. run_identity_test.go carries the resume case; these are the other
// three sites, and the gate that keeps a fourth from being written raw.

// A retry names the lost run by id and its package by the bare id the OS
// holds; the prior row's packageId is canonical.
func TestARetryWithABareRequestIdReadsThePriorRun(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	first, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":                 first.DeploymentId,
		"packageId":          "v1:platform:package:abc",
		"status":             StatusAbandoned,
		"sourceVersion":      "sha-abc123",
		"snapshotArtifactId": "blob://packages/snapshots/snap.tar.gz",
	}}

	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:        "abc",
		Actor:            mayDeployDsl(),
		Confirmed:        true,
		FromDeploymentId: first.DeploymentId,
		Placements:       firstDeployPlacements(),
	})
	if err != nil {
		t.Fatalf("a bare request id against a canonical prior row must retry, got: %v", err)
	}
	if out.Status != StatusSucceeded {
		t.Fatalf("the retry must deploy, got %q", out.Status)
	}
	if len(h.publisher.snapshotReads) != 1 {
		t.Fatalf("the retry must read the prior run's snapshot, got %v", h.publisher.snapshotReads)
	}
}

// A cancel from a deployable's page names the source by its bare id.
func TestCancellingWithABareRequestIdIsNotRefusedAsAnotherSource(t *testing.T) {
	i, engine := cancelHarness(t, StatusAwaitingConfirm)
	args := cancelArgs()
	args["packageId"] = "abc"
	nodes, err := i.handleCancelDeployment(callerCtx("v1:identity:user:someone"), args, 0)
	if err != nil {
		t.Fatalf("a bare packageId against a canonical row must cancel, got: %v", err)
	}
	if got := replyPayload(t, nodes)["status"]; got != StatusCancelled {
		t.Fatalf("want %q, got %v", StatusCancelled, got)
	}
	if !hasCall(engine.statements(), "mutation closePackageDeployment(") {
		t.Fatalf("the parked run was not closed: %v", engine.statements())
	}
}

// THE CONTROL for the cancel: a bare id naming a DIFFERENT source is still
// refused. Comparing through the short id must not make every source equal.
func TestCancellingWithABareIdOfAnotherSourceIsStillRefused(t *testing.T) {
	i, engine := cancelHarness(t, StatusAwaitingConfirm)
	args := cancelArgs()
	args["packageId"] = "zzz"
	_, err := i.handleCancelDeployment(callerCtx("v1:identity:user:someone"), args, 0)
	if err == nil || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("another source's bare id must still be refused, got: %v", err)
	}
	if hasCall(engine.statements(), "mutation closePackageDeployment(") {
		t.Fatal("a refused cancel closed the row anyway")
	}
}

// A rollback from the timeline names the package by its bare id.
func TestRollbackWithABareRequestIdRestoresThePriorRun(t *testing.T) {
	prior := map[string]any{
		"id":        "v1:platform:packageDeployment:old",
		"packageId": "v1:platform:package:abc",
		"status":    StatusSucceeded,
		"deployables": []any{
			map[string]any{"name": "storefront", "siteId": "v1:platform:site:s1", "bundleRef": "blob://sites/s1/v1/"},
		},
	}
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{prior}

	res, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "abc",
		DeploymentId: "v1:platform:packageDeployment:old",
		Actor:        mayDeployDsl(),
	})
	if err != nil {
		t.Fatalf("a bare request id against a canonical prior row must roll back, got: %v", err)
	}
	if res == nil || len(h.publisher.repointed) != 1 {
		t.Fatalf("the site must be repointed: %v", h.publisher.repointed)
	}
}

// sameShortId is pure; its edges are stated directly. It is total in the way
// BareShortId is: a value it cannot decompose compares as itself.
func TestSameShortIdCollapsesTheTwoSpellings(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1:platform:package:abc", "abc", true},
		{"abc", "v1:platform:package:abc", true},
		{"v1:platform:package:abc", "v1:platform:package:abc", true},
		{"abc", "abc", true},
		{" abc ", "v1:platform:package:abc", true},
		{"v1:platform:package:abc", "zzz", false},
		{"v1:platform:package:abc", "", false},
		{"", "", false},
	} {
		if got := sameShortId(c.a, c.b); got != c.want {
			t.Errorf("sameShortId(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// THE GATE. No compare in this package may set a stored `packageId` against a
// request value raw. It scans every non-test source for a line that reads the
// field and compares it on the same line; a compare against the empty string
// is a presence check, not a spelling one, and is exempt. A compare split
// across lines is out of its reach, which is the honest limit of a textual
// gate -- and the reason the four sites share ONE helper rather than four
// local strips.
func TestNoRawPackageIdCompareRemains(t *testing.T) {
	reads := regexp.MustCompile(`"packageId"\)`)
	compare := regexp.MustCompile(`(!=|==)\s*(\S+)`)
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for n, line := range strings.Split(string(src), "\n") {
			if !reads.MatchString(line) || strings.Contains(line, "sameShortId(") {
				continue
			}
			m := compare.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[2], `""`) {
				continue
			}
			offenders = append(offenders, path+":"+strconv.Itoa(n+1)+": "+strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("a stored packageId is canonical and a request id is bare; compare through sameShortId (memql#5291):\n%s",
			strings.Join(offenders, "\n"))
	}
}
