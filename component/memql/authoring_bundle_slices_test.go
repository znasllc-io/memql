package memql

// authoring_bundle_slices_test.go -- unit coverage for the deployment-kind
// bundle slicers + the fail-loud unrecognized-construct backstop (epic
// memql#2354 E1 / #2372). Pure -- no live DB, no automations package (the
// automation-compile hook is exercised by the external e2e test in
// authoring_deployment_bundle_test.go, which links component/automations).

import (
	"strings"
	"testing"
)

// A minimal capability that reconciles against the Go vocabulary (fs.readFile is
// a declared read-class verb).
const bundleCapabilitySrc = `@sideEffect("read")
@description("Read a config file on the runner.")
capability fs.readFile {
  args {
    path string @required
  }
}`

// A minimal action that resolves its bare verb through the file-top capability
// import and type-checks against the catalog schema.
const bundleActionSrc = `use capabilities.fs.{ readFile }

@description("Read a config file on the deploy runner.")
action readConfig {
  args {
    path string @required
  }
  capability readFile(path: args.path)
}`

// SplitBundleSource must classify EVERY deployment-style kind -- concept,
// function family, shape/spec/trait, action, capability -- and the fail-loud
// backstop must surface an unclassifiable region rather than dropping it.
func TestSplitBundleSource_CoversDeploymentKinds(t *testing.T) {
	// Interleave the kinds so ordering is genuinely mixed in the source.
	src := strings.Join([]string{
		bundleCapabilitySrc,
		sessionConceptSrc,
		bundleActionSrc,
		sessionMutationSrc,
		sessionSpecSrc,
	}, "\n\n")

	got := SplitBundleSource(src)
	kindByName := map[string]string{}
	for _, c := range got {
		kindByName[c.Name] = c.Kind
	}

	want := map[string]string{
		"fs.readFile":             "capability",
		"mcpWidget":               "concept",
		"readConfig":              "action",
		"mutationCreateMcpWidget": "mutation",
		"mcpSessSpec":             "spec",
	}
	for name, kind := range want {
		if kindByName[name] != kind {
			t.Errorf("construct %q classified as %q, want %q (all: %+v)", name, kindByName[name], kind, kindByName)
		}
	}
	// No unrecognized region in a clean bundle.
	for _, c := range got {
		if c.Kind == unrecognizedConstructKind {
			t.Errorf("clean bundle produced an unrecognized construct: %+v", c)
		}
	}
}

// The action slice must inherit the file-top `use capabilities.*` imports so the
// downstream loader can resolve the bare capability verb.
func TestExtractActionBundleSlices_PrependsImports(t *testing.T) {
	slices := extractActionBundleSlices(bundleActionSrc)
	if len(slices) != 1 {
		t.Fatalf("expected 1 action slice, got %d", len(slices))
	}
	if slices[0].Name != "readConfig" {
		t.Errorf("action name = %q, want readConfig", slices[0].Name)
	}
	if !strings.Contains(slices[0].Source, "use capabilities.fs.{ readFile }") {
		t.Errorf("action slice must carry the file-top capability import, got:\n%s", slices[0].Source)
	}
}

// Comment-safe slicing: a keyword inside a comment must NOT be extracted as a
// construct.
func TestExtractActionBundleSlices_IgnoresCommentedKeyword(t *testing.T) {
	src := "// action ghost { should not be sliced }\n" + bundleActionSrc
	slices := extractActionBundleSlices(src)
	if len(slices) != 1 || slices[0].Name != "readConfig" {
		t.Fatalf("comment keyword leaked into slices: %+v", slices)
	}
}

// A capability slice carries the dotted name.
func TestExtractCapabilityBundleSlices_DottedName(t *testing.T) {
	slices := extractCapabilityBundleSlices(bundleCapabilitySrc)
	if len(slices) != 1 || slices[0].Name != "fs.readFile" {
		t.Fatalf("expected one fs.readFile capability slice, got %+v", slices)
	}
}

// ValidateBundle compiles a real action + capability bundle to per-construct OK
// with zero skips -- the Gate-1 sandbox now has action + capability compile
// cases (deliverable 2).
func TestValidateBundle_ActionAndCapabilityCompile(t *testing.T) {
	report := ValidateBundle(bundleCapabilitySrc + "\n\n" + bundleActionSrc)
	if !report.OK {
		t.Fatalf("expected OK, got %+v", report.Diagnostics)
	}
	byName := map[string]SandboxDiagnostic{}
	for _, d := range report.Diagnostics {
		byName[d.Name] = d
		if d.Skipped {
			t.Errorf("no construct should be skipped, got skipped %+v", d)
		}
	}
	if d, ok := byName["fs.readFile"]; !ok || !d.OK {
		t.Errorf("capability fs.readFile diagnostic missing or not OK: %+v", d)
	}
	if d, ok := byName["readConfig"]; !ok || !d.OK {
		t.Errorf("action readConfig diagnostic missing or not OK: %+v", d)
	}
}

// FAIL-LOUD: a top-level construct the splitter cannot classify (a prompt) is
// surfaced as an "unrecognized construct" hard failure -- never silently
// dropped and never validated as OK alongside the rest.
func TestValidateBundle_UnrecognizedConstructHardFails(t *testing.T) {
	promptSrc := `@templateFile("x.tmpl")
prompt sneakyPrompt {
  space object
}`
	report := ValidateBundle(sessionConceptSrc + "\n\n" + promptSrc)
	if report.OK {
		t.Fatal("a bundle carrying an unrecognized construct must not validate as OK")
	}
	var found bool
	for _, d := range report.Diagnostics {
		if d.Kind == unrecognizedConstructKind {
			found = true
			if d.OK || d.Skipped {
				t.Errorf("unrecognized construct must be a hard failure (!OK, !Skipped), got %+v", d)
			}
			if !strings.Contains(d.Error, "unrecognized construct") {
				t.Errorf("unrecognized diagnostic should explain itself, got %q", d.Error)
			}
			if !strings.Contains(d.Name, "prompt sneakyPrompt") {
				t.Errorf("unrecognized diagnostic name should excerpt the header line, got %q", d.Name)
			}
		}
	}
	if !found {
		t.Errorf("expected an unrecognized-construct diagnostic, got %+v", report.Diagnostics)
	}
	// The concept still compiled cleanly -- the fail-loud posture rejects the
	// bundle without hiding the good constructs' diagnostics.
	for _, d := range report.Diagnostics {
		if d.Name == "mcpWidget" && !d.OK {
			t.Errorf("the valid concept should still report OK, got %+v", d)
		}
	}
}

// Mixed bundle with ONE bad construct: the bad action reports its failure while
// the sibling capability + action validate cleanly (deliverable 5c).
func TestValidateBundle_MixedBundleIsolatesTheBadConstruct(t *testing.T) {
	// badArgs passes an undeclared arg (`bogus`) to the CLOSED fs.readFile
	// capability -- a strict arg-typing rejection at Gate-1.
	badActionSrc := `use capabilities.fs.{ readFile }

@description("Broken: passes an undeclared arg to a closed capability.")
action badArgs {
  args {
    p string @required
  }
  capability readFile(path: args.p, bogus: args.p)
}`
	report := ValidateBundle(bundleCapabilitySrc + "\n\n" + bundleActionSrc + "\n\n" + badActionSrc)
	if report.OK {
		t.Fatal("a bundle with one broken action must not validate as OK")
	}
	byName := map[string]SandboxDiagnostic{}
	for _, d := range report.Diagnostics {
		byName[d.Name] = d
	}
	if d := byName["readConfig"]; !d.OK {
		t.Errorf("the good action should still be OK, got %+v", d)
	}
	if d := byName["fs.readFile"]; !d.OK {
		t.Errorf("the good capability should still be OK, got %+v", d)
	}
	if d, ok := byName["badArgs"]; !ok || d.OK || d.Skipped {
		t.Errorf("the broken action must be a reported hard failure, got %+v (ok=%v)", d, ok)
	}
}
