package k3d

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// up_db_operand_image_test.go -- znasllc-io/memql#4063.
//
// `k3d.up` overrides the overlay's image names so an INSTALL pulls published
// images instead of the `:local` ones `make dev` builds (memql#3572). It
// discovers those names by matching `memql-*` in the overlay, deliberately, so
// that a node added tomorrow is covered without editing this script.
//
// CloudNativePG then added a name that matches the same pattern and is NOT a
// node: `memql-db`, the database OPERAND. The sweep silently widened to cover
// it, and the operand is versioned on the POSTGRESQL axis rather than the
// engine's -- so a v0.19.0 install asked for `memql-db:0.19.0` and CNPG's
// admission webhook refused the Cluster outright:
//
//	Cluster.postgresql.cnpg.io "memql-db" is invalid: spec.imageName:
//	Invalid value: "ghcr.io/znasllc-io/memql-db:0.19.0":
//	Unsupported PostgreSQL version. Versions 13 or newer are supported
//
// The Cluster is therefore never created, `memql-db-rw` never resolves, every
// node fails readiness against `no such host`, and the install reports only
// that `workloadsReady` did not hold -- a verdict that names nothing and points
// at nothing. That distance between cause and symptom is why this is a test and
// not a comment.
//
// The property: node images take the ENGINE tag, the operand takes its OWN.

// dbOperandOverride returns the emitted override line for `memql-db`.
func dbOperandOverride(t *testing.T, block string) string {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "memql-db=") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func TestImageOverridesDoNotStampTheEngineTagOnTheDatabase(t *testing.T) {
	block := upEmit(t, "memql.localhost", "ghcr.io/znasllc-io", "0.19.0", "kustomize_image_overrides")

	got := dbOperandOverride(t, block)
	if got == "" {
		t.Fatalf("no memql-db override emitted at all; the operand would keep the "+
			"overlay's `16-dev`, which is a `make db-image` artifact an installer never built:\n%s", block)
	}
	if strings.Contains(got, ":0.19.0") {
		t.Errorf("the database operand was stamped with the ENGINE tag: %q\n"+
			"CNPG parses a PostgreSQL version off spec.imageName and refuses what it "+
			"cannot read, so this is rejected by the Cluster admission webhook and no "+
			"database is ever created", got)
	}

	// The positive half, stated as the RULE rather than as one literal tag, so
	// bumping the pinned operand does not have to touch this assertion.
	tag := got[strings.LastIndex(got, ":")+1:]
	if !regexp.MustCompile(`^[0-9]`).MatchString(tag) {
		t.Errorf("operand tag %q does not start with the PostgreSQL major; CNPG reads "+
			"the version off the front of the tag", tag)
	}
}

// The other half: widening the operand's treatment must not have narrowed the
// node sweep. Every node image still takes the engine tag.
func TestImageOverridesStillCoverEveryNodeWithTheEngineTag(t *testing.T) {
	block := upEmit(t, "memql.localhost", "ghcr.io/znasllc-io", "0.19.0", "kustomize_image_overrides")

	for _, node := range []string{"bff", "identity", "agent", "planner", "workbench", "mcp", "edge"} {
		want := "memql-" + node + "=ghcr.io/znasllc-io/memql-" + node + ":0.19.0"
		if !strings.Contains(block, want) {
			t.Errorf("node override missing %q:\n%s", want, block)
		}
	}
}

// A tag CNPG cannot read is refused up front, where the operator is standing,
// rather than several steps later as "workloadsReady did not hold".
func TestUpRefusesADbImageTagCnpgCannotRead(t *testing.T) {
	for _, bad := range []string{"0.19.0", "v16.15", "latest"} {
		out, err := exec.Command("bash", upDomainScript(t),
			"--db-image-tag="+bad,
			"--repo-root=/nonexistent-on-purpose",
		).CombinedOutput()
		if err == nil {
			t.Errorf("--db-image-tag=%q was accepted:\n%s", bad, out)
			continue
		}
		if !strings.Contains(string(out), "PostgreSQL major") {
			t.Errorf("--db-image-tag=%q was rejected, but not for the reason an operator "+
				"needs to hear:\n%s", bad, out)
		}
	}
}

func TestUpDeclaresDbImageTagParam(t *testing.T) {
	out, err := exec.Command("bash", upDomainScript(t), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "db-image-tag") {
		t.Errorf("--print-spec does not declare --db-image-tag\n%s", out)
	}
}
