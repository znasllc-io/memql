package memql

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE PRECONDITION BEHIND THE PACK FLIP'S INTERNAL-ORIGIN STAMP (Connect
// Shopify design, section 9).
//
// call_origin_conformance_test.go admits component/memql, and its reason names
// the Modules pack flip as REQUEST-DERIVED: the stamp runs on a context built
// from an inbound request. What earns it is that the stamp sits downstream of
// AuthorizeSetPackEnabled in the same function, and is applied inline so the
// marked context dies at the one Execute. Both halves are checked here rather
// than trusted, because the allowlist entry is what a reviewer reads instead
// of the code.

const packFlipSource = "module_registry.go"

func TestThePackFlipStampsInternalOriginOnceInSetPackEnabled(t *testing.T) {
	src := readPackFlipSource(t)
	stamps := regexp.MustCompile(`ContextWithInternalOrigin\(`).FindAllStringIndex(src, -1)
	if len(stamps) != 1 {
		t.Fatalf("%s stamps internal origin %d times; the allowlist reason argues for exactly ONE, "+
			"in SetPackEnabled. Each other stamp is a separate decision and needs its own.",
			packFlipSource, len(stamps))
	}
	if fn := functionBodyContaining(src, stamps[0][0]); fn != "SetPackEnabled" {
		t.Fatalf("internal origin is stamped inside %q, not SetPackEnabled", fn)
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "ContextWithInternalOrigin(") &&
			regexp.MustCompile(`^\s*\w+\s*(:?)=\s*\w*\.?ContextWithInternalOrigin\(`).MatchString(line) {
			t.Fatalf("the stamp is ASSIGNED rather than passed inline:\n  %s\n"+
				"That marks the request's context for everything after it (memql#2989). "+
				"Pass it as the argument to the one Execute.", strings.TrimSpace(line))
		}
	}

	// The gate is called in the same function, before the stamp, and its
	// refusal returns before the stamp.
	fnStart := regexp.MustCompile(`(?m)^func \(e \*MemQLEngine\) SetPackEnabled\(`).FindStringIndex(src)
	if fnStart == nil {
		t.Fatal("SetPackEnabled is gone; the allowlist reason names it")
	}
	body := src[fnStart[0]:stamps[0][0]]
	gate := strings.Index(body, "AuthorizeSetPackEnabled(ctx, ")
	if gate < 0 {
		t.Fatal("SetPackEnabled no longer calls AuthorizeSetPackEnabled before its stamp. Without " +
			"the gate the allowlist entry is a standing internal-origin permission on a request context.")
	}
	if !strings.Contains(body[gate:], "if refusal != nil {") {
		t.Fatal("SetPackEnabled does not return on the gate's refusal before its stamp")
	}
}

// The behavioural half: a refused caller never reaches Execute. The engine
// here has no database and no registry, so a write reached by mistake fails
// or panics instead of returning the refusal.
func TestARefusedPackFlipNeverReachesTheEngine(t *testing.T) {
	const storefront = "modregtest-unreached-storefront"
	testStorefrontPack(t, storefront)
	e := &MemQLEngine{}
	for _, tc := range []struct {
		role auth.Role
		pack string
	}{
		{auth.RoleDeveloper, "referencepack"},
		{auth.RoleAdmin, storefront},
		{auth.RoleReader, storefront},
	} {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-x", Role: tc.role})
		_, _, refusal, err := e.SetPackEnabled(ctx, tc.pack, true, "")
		if err != nil || refusal == nil || refusal.Code != moduleCodePermissionDenied {
			t.Errorf("%s flipping %s: refusal=%+v err=%v; want a permission refusal and no write", tc.role, tc.pack, refusal, err)
		}
	}
	if _, _, refusal, err := e.SetPackEnabled(context.Background(), storefront, true, ""); err != nil ||
		refusal == nil || refusal.Code != moduleCodeUnauthenticated {
		t.Errorf("an unauthenticated flip: refusal=%+v err=%v", refusal, err)
	}
}

func readPackFlipSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(packFlipSource)
	if err != nil {
		t.Fatalf("cannot read %s: %v -- it holds the pack flip's internal-origin stamp", packFlipSource, err)
	}
	return string(raw)
}

// functionBodyContaining names the func whose body encloses an offset: the
// nearest preceding top-level `func` declaration.
func functionBodyContaining(src string, offset int) string {
	decls := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`).FindAllStringSubmatchIndex(src[:offset], -1)
	if len(decls) == 0 {
		return ""
	}
	last := decls[len(decls)-1]
	return src[last[2]:last[3]]
}
