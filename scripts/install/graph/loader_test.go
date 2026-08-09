// loader_test.go -- install(12), memql#3369.
//
// These tests pin the LOADER's refusals. The graph documents are data, and the
// loader is the only thing standing between a plausible-looking JSON edit and
// an install that skips a verification, tears down the wrong thing, or hangs on
// a dependency that does not exist. Every refusal below is written as a
// deliberately-broken fixture so the failure mode is named, not implied.
package graph

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// --------------------------------------------------------------------------
// fixtures
// --------------------------------------------------------------------------

// minimalInstall is a two-step install graph that LOADS. Each negative test
// below mutates exactly one thing about it, so the delta is the cause.
const minimalInstall = `{
  "name": "t",
  "kind": "install",
  "description": "fixture",
  "steps": [
    {
      "id": "probe",
      "script": "install.detect",
      "description": "read-only probe",
      "readOnly": true,
      "elevation": "none",
      "verify": {"kind": "scriptOk"}
    },
    {
      "id": "place",
      "script": "install.binary",
      "description": "mutating placement",
      "dependsOn": ["probe"],
      "elevation": "none",
      "receipt": "binary",
      "preExistingPath": "result.preExisting",
      "verify": {"kind": "resultTrue", "field": "result.installed"}
    }
  ]
}`

func mustLoad(t *testing.T, src string) *Graph {
	t.Helper()
	g, err := Load([]byte(src), "fixture")
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	return g
}

// loadErr loads a fixture built by mutating minimalInstall through fn and
// returns the error, failing when the load unexpectedly succeeded.
func loadErr(t *testing.T, fn func(m map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(minimalInstall), &m); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if _, err := Load(b, "fixture"); err != nil {
		return err.Error()
	}
	t.Fatal("Load accepted a graph it must refuse")
	return ""
}

func steps(m map[string]any) []any { return m["steps"].([]any) }
func step(m map[string]any, i int) map[string]any {
	return steps(m)[i].(map[string]any)
}

// --------------------------------------------------------------------------
// the happy path
// --------------------------------------------------------------------------

func TestLoadAcceptsAWellFormedGraph(t *testing.T) {
	g := mustLoad(t, minimalInstall)
	if g.Kind != KindInstall {
		t.Errorf("Kind = %q, want %q", g.Kind, KindInstall)
	}
	if len(g.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(g.Steps))
	}
	if g.Step("place") == nil {
		t.Error("Step(\"place\") returned nil")
	}
	if g.Step("nope") != nil {
		t.Error("Step of an unknown id must be nil")
	}
}

// --------------------------------------------------------------------------
// refusals
// --------------------------------------------------------------------------

// A step with no verify is the whole reason this loader exists: an unverified
// step reports success on the strength of an exit code the script may not even
// control, and the next step builds on a lie.
func TestLoadRefusesAStepWithNoVerify(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { delete(step(m, 1), "verify") })
	if !strings.Contains(msg, "verify") {
		t.Errorf("error does not mention verify: %s", msg)
	}
	if !strings.Contains(msg, "place") {
		t.Errorf("error does not name the offending step: %s", msg)
	}
}

// scriptOk verifies NOTHING but the exit code. On a read-only probe that is
// honest. On a step that changed the machine it is a rubber stamp: the script
// can exit 0 having placed nothing, and the graph would call that installed.
func TestLoadRefusesScriptOkOnAMutatingStep(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["verify"] = map[string]any{"kind": "scriptOk"}
	})
	if !strings.Contains(msg, "scriptOk") {
		t.Errorf("error does not mention scriptOk: %s", msg)
	}
	if !strings.Contains(msg, "readOnly") {
		t.Errorf("error does not explain the readOnly rule: %s", msg)
	}
}

func TestLoadAcceptsScriptOkOnAReadOnlyStep(t *testing.T) {
	g := mustLoad(t, minimalInstall)
	if got := g.Step("probe").Verify.Kind; got != VerifyScriptOk {
		t.Errorf("probe verify kind = %q, want %q", got, VerifyScriptOk)
	}
}

func TestLoadRefusesADanglingDependency(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["dependsOn"] = []any{"ghost"}
	})
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error does not name the dangling dep: %s", msg)
	}
}

func TestLoadRefusesADuplicateStepID(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { step(m, 1)["id"] = "probe" })
	if !strings.Contains(msg, "duplicate") {
		t.Errorf("error does not say duplicate: %s", msg)
	}
}

func TestLoadRefusesASelfDependency(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["dependsOn"] = []any{"place"}
	})
	if !strings.Contains(msg, "place") {
		t.Errorf("error does not name the step: %s", msg)
	}
}

func TestLoadRefusesACycle(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 0)["dependsOn"] = []any{"place"}
	})
	if !strings.Contains(strings.ToLower(msg), "cycle") {
		t.Errorf("error does not say cycle: %s", msg)
	}
}

func TestLoadRefusesAnUnknownVerifyKind(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["verify"] = map[string]any{"kind": "looksFine", "field": "result.x"}
	})
	if !strings.Contains(msg, "looksFine") {
		t.Errorf("error does not name the unknown kind: %s", msg)
	}
}

func TestLoadRefusesAFieldPredicateWithNoField(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["verify"] = map[string]any{"kind": "resultTrue"}
	})
	if !strings.Contains(msg, "field") {
		t.Errorf("error does not mention field: %s", msg)
	}
}

// A predicate that reads outside result{} would be reading the envelope's own
// bookkeeping (ok / changed / capability), which says nothing about what the
// script did to the machine.
func TestLoadRefusesAFieldOutsideResult(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["verify"] = map[string]any{"kind": "resultTrue", "field": "changed"}
	})
	if !strings.Contains(msg, "result.") {
		t.Errorf("error does not explain the result. prefix: %s", msg)
	}
}

func TestLoadRefusesResultEqualsWithNoValue(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["verify"] = map[string]any{"kind": "resultEquals", "field": "result.tool"}
	})
	if !strings.Contains(msg, "value") {
		t.Errorf("error does not mention value: %s", msg)
	}
}

func TestLoadRefusesAValueOnANonEqualsPredicate(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["verify"] = map[string]any{"kind": "resultTrue", "field": "result.installed", "value": "yes"}
	})
	if !strings.Contains(msg, "value") {
		t.Errorf("error does not mention the stray value: %s", msg)
	}
}

// Elevation is DECLARED, never discovered: a step that quietly needs root is a
// step whose blast radius nobody reviewed.
func TestLoadRefusesAMissingElevation(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { delete(step(m, 1), "elevation") })
	if !strings.Contains(msg, "elevation") {
		t.Errorf("error does not mention elevation: %s", msg)
	}
}

func TestLoadRefusesAnUnknownElevation(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { step(m, 1)["elevation"] = "root-ish" })
	if !strings.Contains(msg, "root-ish") {
		t.Errorf("error does not name the bad elevation: %s", msg)
	}
}

// A receipt is a promise that uninstall can take the artifact back. Without a
// recorded pre-existence signal, uninstall cannot tell "we made this" from
// "this was already here", and the safe reading (refuse) makes the receipt
// useless while the unsafe reading destroys the operator's own tooling.
func TestLoadRefusesAReceiptWithNoPreExistingPath(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { delete(step(m, 1), "preExistingPath") })
	if !strings.Contains(msg, "preExistingPath") {
		t.Errorf("error does not mention preExistingPath: %s", msg)
	}
}

func TestLoadRefusesAPreExistingPathWithoutAReceipt(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 0)["preExistingPath"] = "result.supported"
	})
	if !strings.Contains(msg, "preExistingPath") {
		t.Errorf("error does not mention preExistingPath: %s", msg)
	}
}

func TestLoadRefusesAMalformedPreExistingPath(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) {
		step(m, 1)["preExistingPath"] = "preExisting"
	})
	if !strings.Contains(msg, "preExisting") {
		t.Errorf("error does not quote the bad path: %s", msg)
	}
}

func TestLoadAcceptsANegatedPreExistingPath(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(minimalInstall), &m); err != nil {
		t.Fatal(err)
	}
	step(m, 1)["preExistingPath"] = "!result.cloned"
	b, _ := json.Marshal(m)
	g, err := Load(b, "fixture")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	field, negate, ok := g.Step("place").PreExisting()
	if !ok || field != "cloned" || !negate {
		t.Errorf("PreExisting() = (%q, %v, %v), want (\"cloned\", true, true)", field, negate, ok)
	}
}

func TestLoadAcceptsPreExistingNone(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(minimalInstall), &m); err != nil {
		t.Fatal(err)
	}
	step(m, 1)["preExistingPath"] = PreExistingNone
	b, _ := json.Marshal(m)
	g, err := Load(b, "fixture")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, ok := g.Step("place").PreExisting(); ok {
		t.Error("PreExisting() must report ok=false for the explicit \"none\" declaration")
	}
}

// `reverses` names an install step. It is meaningless on an install graph and
// mandatory reasoning on an uninstall one, so the loader keeps them apart.
func TestLoadRefusesReversesOnAnInstallGraph(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { step(m, 1)["reverses"] = "something" })
	if !strings.Contains(msg, "reverses") {
		t.Errorf("error does not mention reverses: %s", msg)
	}
}

func TestLoadRefusesAReceiptOnAnUninstallGraph(t *testing.T) {
	src := strings.Replace(minimalInstall, `"kind": "install"`, `"kind": "uninstall"`, 1)
	if _, err := Load([]byte(src), "fixture"); err == nil {
		t.Fatal("Load accepted a receipt on an uninstall graph")
	} else if !strings.Contains(err.Error(), "receipt") {
		t.Errorf("error does not mention receipt: %v", err)
	}
}

func TestLoadRefusesAnUnknownGraphKind(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { m["kind"] = "reinstall" })
	if !strings.Contains(msg, "reinstall") {
		t.Errorf("error does not name the bad kind: %s", msg)
	}
}

func TestLoadRefusesUnknownJSONFields(t *testing.T) {
	msg := loadErr(t, func(m map[string]any) { step(m, 1)["retries"] = 3 })
	if !strings.Contains(msg, "retries") {
		t.Errorf("a typo'd or invented key must be rejected, got: %s", msg)
	}
}

// --------------------------------------------------------------------------
// TopoOrder returns WAVES
// --------------------------------------------------------------------------

// Parallelism is a PROPERTY OF THE DATA. TopoOrder hands back waves, so an
// executor runs wave[i] concurrently and waits -- it never re-derives which
// steps are independent, and it cannot get that derivation wrong.
func TestTopoOrderReturnsWaves(t *testing.T) {
	src := `{
	  "name": "t", "kind": "install", "description": "d",
	  "steps": [
	    {"id":"a","script":"install.detect","description":"d","readOnly":true,"elevation":"none","verify":{"kind":"scriptOk"}},
	    {"id":"b","script":"install.detect","description":"d","dependsOn":["a"],"readOnly":true,"elevation":"none","verify":{"kind":"scriptOk"}},
	    {"id":"c","script":"install.detect","description":"d","dependsOn":["a"],"readOnly":true,"elevation":"none","verify":{"kind":"scriptOk"}},
	    {"id":"d","script":"install.detect","description":"d","dependsOn":["b","c"],"readOnly":true,"elevation":"none","verify":{"kind":"scriptOk"}}
	  ]
	}`
	g := mustLoad(t, src)
	waves, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	want := [][]string{{"a"}, {"b", "c"}, {"d"}}
	if len(waves) != len(want) {
		t.Fatalf("got %d waves %v, want %d %v", len(waves), waves, len(want), want)
	}
	for i := range want {
		if strings.Join(waves[i], ",") != strings.Join(want[i], ",") {
			t.Errorf("wave %d = %v, want %v", i, waves[i], want[i])
		}
	}
}

func TestTopoOrderCoversEveryStepExactlyOnce(t *testing.T) {
	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		waves, err := g.TopoOrder()
		if err != nil {
			t.Fatalf("%s: TopoOrder: %v", g.Name, err)
		}
		seen := map[string]int{}
		for _, w := range waves {
			for _, id := range w {
				seen[id]++
			}
		}
		if len(seen) != len(g.Steps) {
			t.Errorf("%s: waves cover %d steps, graph has %d", g.Name, len(seen), len(g.Steps))
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("%s: step %q appears %d times across the waves", g.Name, id, n)
			}
		}
	}
}

// --------------------------------------------------------------------------
// the shipped graph documents
// --------------------------------------------------------------------------

func mustLoadEmbedded(t *testing.T, fn func() (*Graph, error)) *Graph {
	t.Helper()
	g, err := fn()
	if err != nil {
		t.Fatalf("load embedded graph: %v", err)
	}
	return g
}

func TestShippedGraphsLoad(t *testing.T) {
	in := mustLoadEmbedded(t, Install)
	if in.Kind != KindInstall {
		t.Errorf("install.json kind = %q", in.Kind)
	}
	if len(in.Steps) == 0 {
		t.Error("install.json has no steps")
	}
	un := mustLoadEmbedded(t, Uninstall)
	if un.Kind != KindUninstall {
		t.Errorf("uninstall.json kind = %q", un.Kind)
	}
	if len(un.Steps) == 0 {
		t.Error("uninstall.json has no steps")
	}
}

// The graph's params are the graph's half of a contract whose other half is a
// shell script's declared flag surface. Ask the scripts themselves rather than
// trusting a comment: a renamed flag must break the graph, loudly, here.
func TestShippedGraphParamsAreDeclaredFlags(t *testing.T) {
	root := repoRoot(t)
	specCache := map[string]map[string]bool{}

	declared := func(script string) map[string]bool {
		if s, ok := specCache[script]; ok {
			return s
		}
		rel, ok := allowlistPath(t, root)[script]
		if !ok {
			t.Fatalf("script %q is not in the capability allowlist", script)
		}
		// Executed directly via its shebang (never `bash -c`), exactly as the
		// engine's capability-script runner does.
		out, err := exec.Command(filepath.Join(root, rel), "--print-spec").Output() //nolint:gosec // constant flag, allowlisted path
		if err != nil {
			t.Fatalf("%s --print-spec: %v", rel, err)
		}
		var spec struct {
			Params []struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(out, &spec); err != nil {
			t.Fatalf("%s --print-spec is not JSON: %v", rel, err)
		}
		set := map[string]bool{}
		for _, p := range spec.Params {
			set[p.Name] = true
		}
		specCache[script] = set
		return set
	}

	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		for i := range g.Steps {
			s := &g.Steps[i]
			flags := declared(s.Script)
			for name := range s.Params {
				if !flags[name] {
					t.Errorf("%s step %q pins --%s, which %s does not declare (cap_spec_param)",
						g.Name, s.ID, name, s.Script)
				}
			}
		}
	}
}

// --------------------------------------------------------------------------
// shared helpers (also used by graph_test.go and dsl_agreement_test.go)
// --------------------------------------------------------------------------

// repoRoot walks up from the test's working directory to the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "lib", "capability.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the repository root")
		}
		dir = parent
	}
}

// allowlistEntryRe matches one `"id": "scripts/...sh",` line of the Go
// capability-script allowlist.
var allowlistEntryRe = regexp.MustCompile(`"([A-Za-z0-9_.]+)":\s*"(scripts/[^"]+\.sh)"`)

// allowlistPath parses component/automations/steps/capability_script.go for the
// REAL allowlist rather than restating it. A graph may only name a script the
// engine would actually agree to run, and the only way to be sure of that is to
// read the boundary itself.
func allowlistPath(t *testing.T, root string) map[string]string {
	t.Helper()
	src := filepath.Join(root, "component", "automations", "steps", "capability_script.go")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	body := string(b)
	start := strings.Index(body, "var capabilityScriptAllowlist = map[string]string{")
	if start < 0 {
		t.Fatal("capabilityScriptAllowlist not found -- this test must be repointed")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of capabilityScriptAllowlist")
	}
	out := map[string]string{}
	for _, m := range allowlistEntryRe.FindAllStringSubmatch(body[start:start+end], -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("parsed zero allowlist entries -- the parser is broken")
	}
	return out
}
