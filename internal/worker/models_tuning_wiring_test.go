package worker

import (
	"bytes"
	"go/ast"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

func tuningPolicy(t *testing.T, doc string) *tools.Policy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p
}

// TestModelsRequestCarriesThePolicysTuning. The request is what every
// discovery in this package is asked, so what the policy says about
// runtimes, residency and concurrency has to be in it.
func TestModelsRequestCarriesThePolicysTuning(t *testing.T) {
	p := tuningPolicy(t, `models:
  allow:
    - qwen3.8:27b
  parallel: 2
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h
      working_context: 16384
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
`)
	req := modelsRequest(p)
	if len(req.Allow) != 1 || req.Allow[0] != "qwen3.8:27b" {
		t.Errorf("allow = %v", req.Allow)
	}
	if len(req.Runtimes) != 1 || req.Runtimes[0].Name != "lmstudio" {
		t.Errorf("runtimes = %+v, want one named lmstudio", req.Runtimes)
	}
	if req.Parallel != 2 {
		t.Errorf("parallel = %d, want 2", req.Parallel)
	}
	want := models.Resident{ID: "qwen3.8:27b", KeepAlive: models.KeepAlive{Set: true, For: 2 * time.Hour}, WorkingContext: 16384}
	if len(req.Resident) != 1 || req.Resident[0] != want {
		t.Errorf("resident = %+v, want [%+v]", req.Resident, want)
	}
}

// TestEveryDiscoveryIsBuiltInOnePlace. Seven commands and run paths probe
// this machine, and `memql worker models` promises that what it prints is
// what the cluster would be told. If one of them builds its own
// Discoverer or its own Request it stops hearing about the tuning, and the
// disagreement is silent: the worker advertises one ceiling while the
// command that exists to explain it prints another.
func TestEveryDiscoveryIsBuiltInOnePlace(t *testing.T) {
	fset, files := workerSources(t)
	var discovererLits, requestLits []string
	callers := map[string]int{}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "models" {
					return true
				}
				pos := fset.Position(x.Pos())
				where := pos.Filename + ":" + strconv.Itoa(pos.Line)
				switch sel.Sel.Name {
				case "Discoverer":
					discovererLits = append(discovererLits, where)
				case "Request":
					requestLits = append(requestLits, where)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "newDiscoverer" {
					callers[path]++
				}
			}
			return true
		})
	}
	if len(discovererLits) != 1 || !strings.HasPrefix(discovererLits[0], "modelinventory.go:") {
		t.Errorf("models.Discoverer literals at %v; want exactly one, inside newDiscoverer in modelinventory.go", discovererLits)
	}
	if len(requestLits) != 1 || !strings.HasPrefix(requestLits[0], "modelinventory.go:") {
		t.Errorf("models.Request literals at %v; want exactly one, inside modelsRequest in modelinventory.go", requestLits)
	}
	// The scan reports its own coverage, for the reason the Runner scan
	// does: a walk that found nothing would pass every assertion above.
	var missing []string
	for _, site := range []string{"cli.go", "pair.go", "models_cmd.go", "inference_cmd.go", "probe_cmd.go", "runtime_cmd.go", "modelinventory.go"} {
		if callers[site] == 0 {
			missing = append(missing, site)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("no newDiscoverer call in %v (found %v): the scan is not looking where discoverers are built", missing, callers)
	}
}

// TestPolicyProblemsAreLoggedAtLoadAndReload. A setting that silently did
// nothing is indistinguishable from one that worked, so the worker says so
// at the two moments the owner could have changed it: start and SIGHUP.
func TestPolicyProblemsAreLoggedAtLoadAndReload(t *testing.T) {
	p := tuningPolicy(t, "models:\n  parallel: 500\n  resident:\n    - id: m\n      keep_alive: 0\n")
	var buf bytes.Buffer
	logLevelProblems(slog.New(slog.NewJSONHandler(&buf, nil)), p)
	out := buf.String()
	for _, want := range []string{
		"policy.yaml models has a problem",
		"models.parallel: 500 is outside 1 to 64",
		"unloads the model after every call",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not say %q:\n%s", want, out)
		}
	}
}

// TestNewDiscovererReadsTheManagedUnit. The ceiling a fresh install
// advertises is the one its unit gives Ollama, with no second place to set
// it: newDiscoverer is what hands discovery the unit.
func TestNewDiscovererReadsTheManagedUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	d := newDiscoverer()
	if d.ManagedParallel == nil {
		t.Fatal("newDiscoverer leaves discovery without the managed unit")
	}
	if got := d.ManagedParallel(); got != 0 {
		t.Errorf("no unit on this machine -> %d, want 0", got)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "memql-ollama.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := d.ManagedParallel(); got != 3 {
		t.Errorf("a unit declaring 3 slots -> %d, want 3", got)
	}
}
