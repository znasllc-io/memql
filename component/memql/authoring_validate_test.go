package memql

// authoring_validate_test.go -- tests for the cockpit-facing authoring surface
// added in issue memql#2128 / C1: the read-only ValidateBundle entry point and
// the validate->inject->callable->never-shadow-core contract the gRPC
// AuthoringSessionDefineBundle op relies on. Pure -- no live DB.

import (
	"testing"
)

// ValidateBundle on a well-formed concept+mutation bundle reports OK with a
// clean per-construct diagnostic for each construct, and mutates NOTHING (it is
// the pure Gate-1 sandbox).
func TestValidateBundle_OK(t *testing.T) {
	report := ValidateBundle(sessionConceptSrc + "\n\n" + sessionMutationSrc)
	if !report.OK {
		t.Fatalf("expected OK, got %+v", report)
	}
	names := map[string]SandboxDiagnostic{}
	for _, d := range report.Diagnostics {
		names[d.Name] = d
	}
	if d, ok := names["mcpWidget"]; !ok || !d.OK {
		t.Errorf("mcpWidget diagnostic missing or not OK: %+v", d)
	}
	if d, ok := names["mutationCreateMcpWidget"]; !ok || !d.OK {
		t.Errorf("mutationCreateMcpWidget diagnostic missing or not OK: %+v", d)
	}
}

// The validate-fail path: a syntactically broken construct comes back OK=false
// with a populated, non-skipped diagnostic explaining the failure.
func TestValidateBundle_FailCarriesDiagnostics(t *testing.T) {
	report := ValidateBundle(`spec actorEnvelope broken { return role == }`)
	if report.OK {
		t.Fatal("expected OK=false for a broken bundle")
	}
	if len(report.Diagnostics) == 0 {
		t.Fatal("expected at least one diagnostic")
	}
	var hardFail bool
	for _, d := range report.Diagnostics {
		if !d.OK && !d.Skipped {
			if d.Error == "" {
				t.Errorf("hard-fail diagnostic missing an error message: %+v", d)
			}
			hardFail = true
		}
	}
	if !hardFail {
		t.Errorf("expected a non-skipped hard failure, got %+v", report.Diagnostics)
	}
}

// An empty / unrecognizable bundle returns a typed OK=false report rather than
// panicking or returning a bare nil -- so the gRPC surface always has something
// to send back.
func TestValidateBundle_EmptyReportsTyped(t *testing.T) {
	report := ValidateBundle("   \n  ")
	if report.OK {
		t.Error("empty bundle should not be OK")
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Error == "" {
		t.Errorf("empty bundle should carry one explanatory diagnostic, got %+v", report.Diagnostics)
	}
}

// The inject-success contract behind AuthoringSessionDefineBundle: a valid
// function-family bundle becomes callable BY NAME in the owner's session
// overlay, and a session construct whose name collides with a CORE construct
// never shadows the core one (core-first). This is the exact path the gRPC
// handler drives via AuthorSessionBundle + the engine overlay.
func TestSessionDefine_CallableAndNeverShadowsCore(t *testing.T) {
	// Seed a core function whose name a session bundle will try to redefine.
	e := &MemQLEngine{functions: newFunctionRegistry()}
	coreFn := &Function{Name: "sharedName", FunctionKind: "query", Enabled: true, ExprSource: "core"}
	if err := e.functions.Upsert(coreFn); err != nil {
		t.Fatalf("seed core: %v", err)
	}

	reg := NewAuthoredRuntimeRegistry()

	// Session-define a net-new construct (callable) and one that collides with
	// the seeded core name (must be dropped from the overlay).
	res, err := AuthorSessionBundle(reg, "owner-1", sessionConceptSrc+"\n\n"+sessionMutationSrc)
	if err != nil {
		t.Fatalf("session-define net-new: %v (diags %+v)", err, res.Diagnostics)
	}
	if !res.OK || len(res.Defined) == 0 {
		t.Fatalf("net-new bundle should define constructs, got %+v", res)
	}

	// Net-new construct is callable by name via the owner's overlay.
	overlay := e.buildAuthoredFunctionOverlay("owner-1", reg)
	if got, _ := overlay.Get("mutationCreateMcpWidget"); got == nil {
		t.Error("session-defined construct is not callable by name in the owner overlay")
	}

	// Now register a session construct colliding with the core name and confirm
	// the overlay keeps the CORE definition (never shadowed).
	mustRegister(t, reg, &AuthoredConstruct{
		OwnerUserId: "owner-1", Kind: "query", Name: "sharedName", Version: 1, Status: AuthoredActive,
		Compiled: &Function{Name: "sharedName", FunctionKind: "query", Enabled: true, ExprSource: "authored"},
	})
	overlay = e.buildAuthoredFunctionOverlay("owner-1", reg)
	if got, _ := overlay.Get("sharedName"); got == nil || got.ExprSource != "core" {
		t.Errorf("session construct must NOT shadow core; overlay sharedName = %+v", got)
	}

	// A different owner never resolves owner-1's session constructs.
	otherOverlay := e.buildAuthoredFunctionOverlay("owner-2", reg)
	if got, _ := otherOverlay.Get("mutationCreateMcpWidget"); got != nil {
		t.Error("another owner must not resolve owner-1's session-defined construct")
	}
}
