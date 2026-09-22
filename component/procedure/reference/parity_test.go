// Package reference is the research sidecar of design D6: reference
// implementations of the two algorithms whose correctness is easiest to get
// subtly wrong, and a harness that runs the SHARED fixtures through both and
// asserts they agree.
//
// It is never product. D6 says an external mining sidecar with Python
// libraries is "kept as a research harness for validating the Go
// implementation against reference algorithms, never as product: it would put
// a second runtime into product-agnostic engine images". Nothing in
// component/procedure imports this package, and the purity gate covers that
// package rather than this directory.
//
// The references are written from the DEFINITIONS rather than from the Go. A
// reference transliterated from the implementation it checks agrees with it by
// construction and proves nothing.
package reference

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/procedure"
)

const installHint = `the reference implementations need python3 on PATH (no packages).
Install it and re-run:  go test ./component/procedure/reference/...`

// runReference pipes a fixture document through a reference script. It reports
// ok=false when python3 is absent, which is the SKIP case: a harness that
// failed when a research sidecar is missing would red the build for everyone.
func runReference(t *testing.T, script, input string) ([]byte, bool) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		return nil, false
	}
	path := filepath.Join(".", script)
	if _, err := os.Stat(path); err != nil {
		return nil, false
	}
	cmd := exec.Command(py, path)
	cmd.Stdin = strings.NewReader(input)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\n%s", script, err, errb.String())
	}
	return out.Bytes(), true
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

type lcsDoc struct {
	Cases []struct {
		Name     string   `json:"name"`
		A        []string `json:"a"`
		B        []string `json:"b"`
		Distance int      `json:"distance"`
	} `json:"cases"`
}

type mineDoc struct {
	Cases []struct {
		Name       string     `json:"name"`
		Sequences  [][]string `json:"sequences"`
		MinSupport int        `json:"minSupport"`
		Gap        int        `json:"gap"`
	} `json:"cases"`
}

// TestParity_TheReferencesAgreeWithTheGoResultsOnTheSharedFixtures is issue
// #5407's acceptance criterion.
func TestParity_TheReferencesAgreeWithTheGoResultsOnTheSharedFixtures(t *testing.T) {
	t.Run("anti-unification distance", func(t *testing.T) {
		raw := readFixture(t, "lcs_fixtures.json")
		out, ok := runReference(t, "ref_lcs.py", raw)
		if !ok {
			t.Skip(installHint)
		}
		var want struct {
			Distances []int `json:"distances"`
		}
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatalf("decode reference output: %v\n%s", err, out)
		}
		var doc lcsDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		if len(want.Distances) != len(doc.Cases) {
			t.Fatalf("reference returned %d distances for %d cases", len(want.Distances), len(doc.Cases))
		}
		for i, c := range doc.Cases {
			_, got := procedure.AntiUnify(arrOf(c.A), arrOf(c.B), procedure.NewHoleNamer())
			if got != want.Distances[i] {
				t.Errorf("case %q: Go distance %d, reference %d", c.Name, got, want.Distances[i])
			}
			if got != c.Distance {
				t.Errorf("case %q: Go distance %d, fixture says %d", c.Name, got, c.Distance)
			}
		}
	})

	t.Run("closed frequent sub-sequences", func(t *testing.T) {
		raw := readFixture(t, "mine_fixtures.json")
		out, ok := runReference(t, "ref_mine.py", raw)
		if !ok {
			t.Skip(installHint)
		}
		var want struct {
			Top [][]string `json:"top"`
		}
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatalf("decode reference output: %v\n%s", err, out)
		}
		var doc mineDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		for i, c := range doc.Cases {
			p := procedure.DefaultParams()
			p.MinSupport, p.Gap = c.MinSupport, c.Gap
			got := procedure.Mine(c.Sequences, p)
			var gotTop []string
			if len(got) > 0 {
				gotTop = got[0].Symbols
			}
			if !equalStrings(gotTop, want.Top[i]) {
				t.Errorf("case %q: Go top %v, reference %v", c.Name, gotTop, want.Top[i])
			}
		}
	})
}

// TestParity_TheHarnessFailsWhenAReferenceDisagrees is the NEGATIVE CONTROL.
// A parity harness that cannot fail is a harness that proves nothing, and
// running it against a deliberately wrong reference is the only way to know it
// can. The wrong reference is committed beside the real ones for exactly this.
func TestParity_TheHarnessFailsWhenAReferenceDisagrees(t *testing.T) {
	raw := readFixture(t, "lcs_fixtures.json")
	out, ok := runReference(t, "ref_lcs_wrong.py", raw)
	if !ok {
		t.Skip(installHint)
	}
	var wrong struct {
		Distances []int `json:"distances"`
	}
	if err := json.Unmarshal(out, &wrong); err != nil {
		t.Fatalf("decode wrong reference output: %v\n%s", err, out)
	}
	var doc lcsDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	disagreements := 0
	for i, c := range doc.Cases {
		_, got := procedure.AntiUnify(arrOf(c.A), arrOf(c.B), procedure.NewHoleNamer())
		if got != wrong.Distances[i] {
			disagreements++
		}
	}
	if disagreements == 0 {
		t.Fatal("the deliberately wrong reference agreed with the Go on every fixture: " +
			"this harness cannot detect a disagreement, so its green means nothing")
	}
}

func arrOf(ss []string) *procedure.Node {
	kids := make([]*procedure.Node, len(ss))
	for i, s := range ss {
		kids[i] = procedure.Lit(s)
	}
	return procedure.Arr(kids...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
