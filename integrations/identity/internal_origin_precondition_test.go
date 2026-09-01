package identity

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// THE PRECONDITION BEHIND THIS PACKAGE'S INTERNAL-ORIGIN ALLOWLIST ENTRY.
//
// call_origin_conformance_test.go admits `integrations/identity` as a
// REQUEST-DERIVED exception -- the fourth -- and the argument is not that the
// context is safe in general. It is that every path reaching the stamp is
// downstream of the cluster-owner gate in the same function, and that the
// stamp is applied inline so the marked context dies at the one Execute.
//
// Both halves are checked here rather than trusted, for the reason
// component/identity/adminops/gate_test.go checks its own: an allowlist entry
// whose stated reason has quietly stopped being true is worse than no entry,
// because the entry is what a reviewer reads instead of the code.

const transferSource = "ownership_transfer.go"

func TestInternalOriginIsStampedOnlyInReassignRow(t *testing.T) {
	src := readTransferSource(t)
	stamps := regexp.MustCompile(`ContextWithInternalOrigin\(`).FindAllStringIndex(src, -1)
	if len(stamps) == 0 {
		t.Fatalf("%s no longer stamps internal origin at all. If that is deliberate, remove the "+
			"`integrations/identity` entry from the allowlist in call_origin_conformance_test.go -- "+
			"an entry nothing needs is a standing permission nobody is checking.", transferSource)
	}
	if len(stamps) != 1 {
		t.Fatalf("%s stamps internal origin %d times; the allowlist entry argues for exactly ONE, "+
			"in reassignRow. Each additional stamp is a separate decision and needs its own.",
			transferSource, len(stamps))
	}
	// The stamp must live inside reassignRow -- the function that writes one
	// row's new owner -- and nowhere else.
	fn := functionBodyContaining(src, stamps[0][0])
	if fn != "reassignRow" {
		t.Fatalf("internal origin is stamped inside %q, not reassignRow. The allowlist entry names "+
			"the write of one row's new owner as the whole of what this package may widen.", fn)
	}
}

// TestTheStampIsInlineAndNeverReassigned is the memql#2989 shape, checked.
//
// `ctx = auth.ContextWithInternalOrigin(ctx)` marks the request's OWN context
// and opens every @serverOnly construct -- and the row-authz write guard -- for
// the rest of that request. The safe form hands the marked context straight to
// the one call that needs it.
func TestTheStampIsInlineAndNeverReassigned(t *testing.T) {
	for _, line := range strings.Split(readTransferSource(t), "\n") {
		if !strings.Contains(line, "ContextWithInternalOrigin(") {
			continue
		}
		if regexp.MustCompile(`^\s*\w+\s*(:?)=\s*\w*\.?ContextWithInternalOrigin\(`).MatchString(line) {
			t.Fatalf("the stamp is ASSIGNED rather than passed inline:\n  %s\n\n"+
				"That marks the context for the rest of the request, which memql#2989 built and "+
				"refuted. Pass it as the argument to the one Execute that needs it.",
				strings.TrimSpace(line))
		}
	}
}

// TestTheClusterOwnerGateGuardsEveryPathToTheStamp asserts the precondition the
// allowlist entry rests on.
//
// It is a source check rather than a behavioural one, and that is a real limit
// worth stating: it proves the gate is present and ordered before the work,
// not that no future refactor routes around it. The behavioural half is
// TestTransferIsAClusterOwnerAction, which calls the handler as four
// non-owner roles and requires a refusal from each.
func TestTheClusterOwnerGateGuardsEveryPathToTheStamp(t *testing.T) {
	src := readTransferSource(t)
	gate := strings.Index(src, "!access.IsClusterOwner()")
	if gate < 0 {
		t.Fatal("handleTransferRowOwnership no longer checks IsClusterOwner. The allowlist entry " +
			"for this package is the claim that it does; without the gate the entry is a " +
			"standing internal-origin permission on a request-derived context.")
	}
	// The only caller of reassignRow must sit after the gate.
	for _, call := range regexp.MustCompile(`i\.reassignRow\(`).FindAllStringIndex(src, -1) {
		if call[0] < gate {
			t.Fatalf("reassignRow is called at offset %d, BEFORE the cluster-owner gate at %d -- "+
				"a path to the stamp that the gate does not cover", call[0], gate)
		}
	}
}

func readTransferSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(transferSource)
	if err != nil {
		t.Fatalf("cannot read %s: %v -- this file is the subject of an internal-origin allowlist "+
			"entry, so its absence is a failure rather than a skip", transferSource, err)
	}
	return string(raw)
}

// functionBodyContaining names the func whose body encloses an offset. Crude
// but sufficient: it finds the nearest preceding `func` declaration.
func functionBodyContaining(src string, offset int) string {
	decls := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`).FindAllStringSubmatchIndex(src[:offset], -1)
	if len(decls) == 0 {
		return ""
	}
	last := decls[len(decls)-1]
	return src[last[2]:last[3]]
}
