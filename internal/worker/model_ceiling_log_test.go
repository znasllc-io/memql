package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// model_ceiling_log_test.go pins the worker's own record of the ceiling it
// advertises. `memql worker models` can only guess the worker's environment;
// the worker knows it, so at start and on every SIGHUP it says, for each model
// it serves, how many requests it takes at once and where that number came
// from. That line is the one that cannot differ from what is advertised.

// ceilingLog is one decoded log line.
type ceilingLog map[string]any

func decodeLog(t *testing.T, logs string) []ceilingLog {
	t.Helper()
	var out []ceilingLog
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var rec ceilingLog
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func servedModel(id, kind, source string, max int) models.Info {
	m := offeredModel(id, models.Attributes{MaxConcurrent: max})
	m.Kind = kind
	m.CeilingSource = source
	return m
}

func TestLogModelCeilings_OneInfoLinePerServedModel(t *testing.T) {
	blocked := servedModel("blocked:7b", models.KindOllama, "the default", 1)
	blocked.Allowed = false
	// In the order Probe returns them: sorted by id.
	inv := servingInventory(
		blocked,
		servedModel("qwen3-embedding:0.6b", models.KindOllama, "OLLAMA_NUM_PARALLEL in the worker's environment", 2),
		servedModel("qwen3.8:27b", models.KindOllama, "OLLAMA_NUM_PARALLEL in the worker's environment", 2),
		servedModel("remote:7b", models.KindOpenAICompatible, "max_concurrent under models.runtimes in policy.yaml", 4),
	)
	logs := &logBuffer{}
	logModelCeilings(slogJSON(logs), inv)

	want := []struct {
		model  string
		max    float64
		source string
	}{
		{"qwen3-embedding:0.6b", 2, "OLLAMA_NUM_PARALLEL in the worker's environment"},
		{"qwen3.8:27b", 2, "OLLAMA_NUM_PARALLEL in the worker's environment"},
		{"remote:7b", 4, "max_concurrent under models.runtimes in policy.yaml"},
	}
	got := decodeLog(t, logs.String())
	if len(got) != len(want) {
		t.Fatalf("%d lines, want one for each of the %d served models:\n%s", len(got), len(want), logs)
	}
	for i, w := range want {
		rec := got[i]
		if rec["level"] != "INFO" || rec["msg"] != "model concurrency ceiling" {
			t.Errorf("line %d is %v %q, want INFO \"model concurrency ceiling\"", i, rec["level"], rec["msg"])
		}
		if rec["model"] != w.model || rec["model_max"] != w.max || rec["source"] != w.source {
			t.Errorf("line %d = model %v, model_max %v, source %q; want %s, %v, %q", i, rec["model"], rec["model_max"], rec["source"], w.model, w.max, w.source)
		}
	}
	if strings.Contains(logs.String(), "blocked:7b") {
		t.Errorf("a model this machine does not serve was logged as having a ceiling:\n%s", logs)
	}
}

// Nothing served is nothing to say: a machine below the hardware floor, or
// with no model allowed, advertises no ceiling.
func TestLogModelCeilings_NothingServedLogsNothing(t *testing.T) {
	below := models.Inventory{
		Floor:  models.FloorVerdict{Reason: "this machine has 8 GB of unified memory; the floor is 16 GB."},
		Models: []models.Info{servedModel("m:7b", models.KindOllama, "the default", 1)},
	}
	notAllowed := servedModel("m:7b", models.KindOllama, "the default", 1)
	notAllowed.Allowed = false
	for name, inv := range map[string]models.Inventory{
		"below the floor": below,
		"none allowed":    servingInventory(notAllowed),
		"no models":       servingInventory(),
		"no inventory":    {},
	} {
		logs := &logBuffer{}
		logModelCeilings(slogJSON(logs), inv)
		if logs.String() != "" {
			t.Errorf("%s: logged %s", name, logs)
		}
	}
}

// announceModelCeilings asks the shared inventory, which is cached, so the
// registration that follows reuses the probe instead of paying for a second.
func TestAnnounceModelCeilings_LogsWhatTheInventoryHolds(t *testing.T) {
	inv := &fakeModelInventory{inv: servingInventory(
		servedModel("qwen3.8:27b", models.KindOllama, "models.parallel in policy.yaml", 2),
	)}
	logs := &logBuffer{}
	announceModelCeilings(context.Background(), slogJSON(logs), inv)
	if logs.count(`"msg":"model concurrency ceiling"`, `"model":"qwen3.8:27b"`, `"model_max":2`, `"source":"models.parallel in policy.yaml"`) != 1 {
		t.Errorf("the ceiling was not logged once:\n%s", logs)
	}
	// And a worker with no inventory at all (a build that serves no models)
	// says nothing rather than failing.
	logs = &logBuffer{}
	announceModelCeilings(context.Background(), slogJSON(logs), nil)
	if logs.String() != "" {
		t.Errorf("a nil inventory logged %s", logs)
	}
}

// ollamaServing is an Ollama at a loopback address that has the given models
// pulled, answering what discovery asks.
func ollamaServing(t *testing.T, ids ...string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		var list []string
		for _, id := range ids {
			list = append(list, fmt.Sprintf(`{"name":%q,"model":%q}`, id, id))
		}
		_, _ = fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(list, ","))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"capabilities":["completion"],"model_info":{"llama.context_length":8192}}`))
	})
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.34.2"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestTheLoggedCeilingIsWhatDiscoveryUsed. The line is only worth anything if
// it names the number the worker advertises and the source discovery really
// read, at start and again after a reload. This drives the real chain: the
// policy file, the request built from it, the discoverer, the shared inventory
// and its cache -- with only Ollama, the hardware floor and the machine's unit
// stubbed.
func TestTheLoggedCeilingIsWhatDiscoveryUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	write := func(doc string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("models:\n  allow: [chat:27b, embed:0.6b]\n")
	policy, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}

	d := newDiscoverer()
	d.OllamaBaseURL = ollamaServing(t, "chat:27b", "embed:0.6b")
	d.FloorFn = func() models.FloorVerdict { return models.FloorVerdict{Met: true} }
	d.ManagedParallel = func() int { return 0 }
	d.Getenv = func(k string) string {
		if k == "OLLAMA_NUM_PARALLEL" {
			return "3"
		}
		return ""
	}
	inv := NewModelInventory(policy, d)

	// At start: the worker's own environment answers.
	logs := &logBuffer{}
	announceModelCeilings(context.Background(), slogJSON(logs), inv)
	for _, id := range []string{"chat:27b", "embed:0.6b"} {
		if logs.count(`"msg":"model concurrency ceiling"`, `"model":"`+id+`"`, `"model_max":3`, `"source":"OLLAMA_NUM_PARALLEL in the worker's environment"`) != 1 {
			t.Errorf("at start, %s is not logged at 3 from the worker's environment:\n%s", id, logs)
		}
	}

	// The owner writes models.parallel and sends SIGHUP: the policy reloads, the
	// request for a fresh look drops the cached inventory, and the line follows.
	write("models:\n  allow: [chat:27b, embed:0.6b]\n  parallel: 2\n")
	if !reloadPolicy(slogJSON(&logBuffer{}), policy) {
		t.Fatal("the policy did not reload")
	}
	inv.Invalidate()
	logs = &logBuffer{}
	announceModelCeilings(context.Background(), slogJSON(logs), inv)
	for _, id := range []string{"chat:27b", "embed:0.6b"} {
		if logs.count(`"model":"`+id+`"`, `"model_max":2`, `"source":"models.parallel in policy.yaml"`) != 1 {
			t.Errorf("after the reload, %s is not logged at 2 from the policy:\n%s", id, logs)
		}
	}
}

// TestEveryRunPathAnnouncesTheCeilingsAtStartAndAtReload. `worker run` and the
// run that follows `worker pair` each start the worker and each handle SIGHUP,
// and the line is only worth anything if it is there at both moments on both
// paths: a path that forgot it would read as a worker with no ceiling. It is
// started with `go`, because it probes Ollama and neither the worker's start
// nor the goroutine that also handles SIGTERM may wait on that.
func TestEveryRunPathAnnouncesTheCeilingsAtStartAndAtReload(t *testing.T) {
	_, files := workerSources(t)
	for _, file := range []string{"cli.go", "pair.go"} {
		f := files[file]
		started := map[*ast.CallExpr]bool{}
		inReload := map[*ast.CallExpr]bool{}
		var all []*ast.CallExpr
		isAnnounce := func(n ast.Node) (*ast.CallExpr, bool) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return nil, false
			}
			id, ok := call.Fun.(*ast.Ident)
			return call, ok && id.Name == "announceModelCeilings"
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := isAnnounce(n); ok {
				all = append(all, call)
			}
			if g, ok := n.(*ast.GoStmt); ok {
				started[g.Call] = true
			}
			if clause, ok := n.(*ast.CaseClause); ok && namesSIGHUP(clause.List) {
				for _, stmt := range clause.Body {
					ast.Inspect(stmt, func(m ast.Node) bool {
						if call, ok := isAnnounce(m); ok {
							inReload[call] = true
						}
						return true
					})
				}
			}
			return true
		})
		var atStart int
		for _, call := range all {
			if !started[call] {
				t.Errorf("%s calls announceModelCeilings without `go`: it probes Ollama, and would hold up what called it", file)
			}
			if !inReload[call] {
				atStart++
			}
		}
		if atStart == 0 {
			t.Errorf("%s never announces the ceilings when the worker starts", file)
		}
		if len(inReload) == 0 {
			t.Errorf("%s never announces the ceilings after a SIGHUP reload", file)
		}
	}
}
