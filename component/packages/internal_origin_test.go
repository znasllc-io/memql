package packages

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// internal_origin_test.go is the PRECONDITION behind this package's entry in
// the ContextWithInternalOrigin allowlist (call_origin_conformance_test.go).
//
// The allowlist entry is not the safety property; this file is. The entry says
// "server-initiated, no caller-supplied identifier in scope"; these tests are
// what make that a fact rather than a claim, in the shape
// component/identity/adminops/gate_test.go established.

// originEngine records the CallOrigin each Execute arrived under.
type originEngine struct {
	origins map[string]auth.CallOrigin
}

func (e *originEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if e.origins == nil {
		e.origins = map[string]auth.CallOrigin{}
	}
	e.origins[callName(query)] = auth.OriginFromContext(ctx)
	return &memql.ExecuteResult{}, nil
}

// TestEveryWriteIsStampedAndEveryReadIsNot is the whole authorization shape of
// this package in one assertion.
//
// The WRITES must be stamped: they reach @serverOnly mutations, and
// OriginClient is the zero value, so an unstamped write is refused with only a
// WARN -- a deploy that hangs with nothing in its timeline.
//
// The READS must NOT be: both concepts declare a composite owner tier, and an
// unstamped read is how row admission stays the tier's decision rather than
// this package's. A stamped read would not error -- it would quietly widen
// what the pipeline can see, which is the failure nobody notices.
func TestEveryWriteIsStampedAndEveryReadIsNot(t *testing.T) {
	e := &originEngine{}
	s := &store{engine: e}
	ctx := context.Background()

	_, _ = s.packageById(ctx, "p")
	_, _ = s.deploymentById(ctx, "d")
	_, _ = s.sitesForPackage(ctx, "p")
	_, _ = s.siteById(ctx, "s")
	_, _ = s.packagesByRepoUrl(ctx, "u")
	_, _ = s.packagesTrackingRepos(ctx)
	_, _, _ = s.artifactBytes(ctx, "a", nil)

	_ = s.advance(ctx, "d", StatusBuilding)
	_ = s.recordDeployedVersion(ctx, "p", "v", false)
	_ = s.recordPackageName(ctx, "p", "n")
	_ = s.recordUpstreamVersion(ctx, "p", "v", true)
	_ = s.bindSiteToPackage(ctx, "s", "p", "d")
	_ = s.setPackageStatus(ctx, "p", "archived")
	_ = s.setSiteStatus(ctx, "s", "archived")
	_ = s.recordReport(ctx, "d", &Report{}, "")

	// Source credentials (epic memql#4885): two more stamped writes, the
	// one stamped read, and the one caller-actor write.
	_, _ = s.sourceCredentialSealedById(ctx, "c")
	_ = s.createSourceCredential(ctx, credentialSeed{CredentialId: "c", Host: "github.com", Label: "l", EncryptedValue: "e", Fingerprint: "f"})
	_ = s.touchSourceCredential(ctx, "c", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	_ = s.revokeSourceCredential(ctx, "c")

	// Placements (epic memql#4885, D8): two more caller-actor writes.
	_ = s.setSiteAccount(ctx, "s", "a")
	_ = s.addCustomDomain(ctx, "s", "www.example.com")

	reads := []string{"packageById", "packageDeploymentById", "sitesForPackage", "siteById", "packagesByRepoUrl", "packagesTrackingRepos", "libraryArtifactById"}
	writes := []string{"advancePackageDeployment", "recordPackageDeployedVersion", "recordPackageName",
		"recordPackageUpstreamVersion", "recordSitePackageOrigin", "setPackageStatus", "setSiteStatus",
		"recordPackageDeploymentReport", "createSourceCredential", "touchSourceCredential"}
	// THE ONE STAMPED READ. sourceCredentialSealedById is @serverOnly because
	// it returns ciphertext -- a client-callable projection of encryptedValue
	// is a ciphertext oracle even for the row's own owner -- so without the
	// stamp the engine could not reach it at all. The stamp admits the
	// CONSTRUCT and does not widen the ROWS: the read path has no
	// internal-origin bypass, and the actor (the package owner, borrowed by
	// resolveCredential) still decides what comes back.
	stampedReads := []string{"sourceCredentialSealedById"}
	// THE CALLER-ACTOR WRITES. revokeSourceCredential is an ordinary owned
	// mutation the write guard decides for the caller; stamping it internal
	// would hand the guard its first escape and let anyone revoke anything.
	// updateSiteAccount and customDomainAdd are the D8 placement writes: the
	// SAME calls the page issues, run by the pipeline under the caller's
	// actor so the account write's guard and the three custom-domain guards
	// decide exactly as they do from the page. Stamped, the pipeline would be
	// a bypass of both, which is what the design says it must not gain.
	callerWrites := []string{"revokeSourceCredential", "updateSiteAccount", "customDomainAdd"}

	for _, name := range reads {
		got, ok := e.origins[name]
		if !ok {
			t.Fatalf("control failed: %s never reached the engine, so this test observed nothing", name)
		}
		if got != auth.OriginClient {
			t.Errorf("%s is a READ and must run under the caller's own origin, got %v", name, got)
		}
	}
	for _, name := range writes {
		got, ok := e.origins[name]
		if !ok {
			t.Fatalf("control failed: %s never reached the engine, so this test observed nothing", name)
		}
		if got != auth.OriginInternal {
			t.Errorf("%s is a @serverOnly WRITE and must be stamped internal, got %v -- unstamped it is refused with only a WARN", name, got)
		}
	}
	for _, name := range stampedReads {
		got, ok := e.origins[name]
		if !ok {
			t.Fatalf("control failed: %s never reached the engine, so this test observed nothing", name)
		}
		if got != auth.OriginInternal {
			t.Errorf("%s is the @serverOnly ciphertext read and must be stamped internal, got %v -- unstamped, every fetch under a credential is refused", name, got)
		}
	}
	for _, name := range callerWrites {
		got, ok := e.origins[name]
		if !ok {
			t.Fatalf("control failed: %s never reached the engine, so this test observed nothing", name)
		}
		if got != auth.OriginClient {
			t.Errorf("%s is an OWNED write the write guard decides for the caller and must NOT be stamped, got %v -- stamped, the guard's internal-origin escape admits any caller against any row", name, got)
		}
	}
}

// TestTheStampNeverEscapesItsCall pins the memql#2879 escalation shape: a
// trusted frame stamps internal, binds it to a variable, and a later frame in
// the same tree runs caller-submitted text on the inherited context.
//
// The structural guarantee is that ContextWithInternalOrigin appears exactly
// once in this package, inline as the argument to one Execute. A source scan
// rather than a behavioural test, because the property is "no other call site
// exists" -- which a behavioural test cannot observe.
func TestTheStampNeverEscapesItsCall(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("%s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ContextWithInternalOrigin" {
				return true
			}
			sites++
			// It must be an ARGUMENT to another call, never the right-hand
			// side of an assignment -- that is what makes it die at the call.
			return true
		})

		// The assignment form is what would let it flow on.
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, rhs := range assign.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ContextWithInternalOrigin" {
					t.Errorf("%s binds ContextWithInternalOrigin to a variable. Keep it INLINE as the argument to the one Execute that needs it, or a later frame inherits the mark (memql#2879).", name)
				}
			}
			return true
		})
	}

	if sites != 1 {
		t.Errorf("want exactly one ContextWithInternalOrigin call site in this package (store.executeInternal), found %d", sites)
	}
}
