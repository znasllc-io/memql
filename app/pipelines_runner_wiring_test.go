package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// pipelines_runner_wiring_test.go -- the workbench node's pipeline runner,
// wired (epic memql#5478, #5493, #5495): built when the node can run steps, a
// NIL INTERFACE when it cannot, and the one thing behind the forward handler's
// four pipeline actions either way.
//
// The bug this exists to stop is the quiet one. A runner that is never
// installed, or installed as a typed nil, looks like a cluster with no
// pipelines: every step answers pipelines_not_configured, or the handler
// calls a nil pointer, while every unit test of the runner stays green.

// pipelinesWiringConfig is a workbench node that can run steps.
func pipelinesWiringConfig() pipelinesteps.Config {
	cfg := pipelinesteps.ConfigFromEnv(func(string) string { return "" })
	cfg.CloneImage = "registry.example/clone@sha256:0000"
	cfg.Namespace = "steps-ns"
	cfg.NodeID = "workbench-a"
	return cfg
}

// pipelinesAPIServer plays the Kubernetes API server: every Job is absent.
// It records the paths it was asked for.
func pipelinesAPIServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// TestTheWorkbenchRunsPipelineStepsOnlyWhenConfigured. A node with a clone
// image and an in-cluster API server gets a runner; any other answers every
// pipeline forward pipelines_not_configured -- and says why ONCE: at Info when
// the node is simply not configured for pipelines, which is a cluster set up
// that way, and at Warn when it is and the API client still cannot be built.
// It builds nothing it will not use: no API client, no blob client.
func TestTheWorkbenchRunsPipelineStepsOnlyWhenConfigured(t *testing.T) {
	srv, _ := pipelinesAPIServer(t)
	for _, tc := range []struct {
		what       string
		cloneImage string
		inCluster  bool
		apiErr     error
		want       bool
		// level and says are the one log line's level and every phrase it
		// must carry.
		level string
		says  []string
	}{
		{what: "configured", cloneImage: "clone:1", inCluster: true, want: true},
		{what: "no clone image", inCluster: true, level: "INFO", says: []string{"MEMQL_PIPELINES_CLONE_IMAGE"}},
		{what: "no in-cluster API", cloneImage: "clone:1", level: "INFO", says: []string{"in-cluster Kubernetes API"}},
		{what: "neither", level: "INFO", says: []string{"MEMQL_PIPELINES_CLONE_IMAGE", "in-cluster Kubernetes API"}},
		{what: "an unbuildable API client", cloneImage: "clone:1", inCluster: true,
			apiErr: errors.New("no_cluster_api: no cluster CA"), level: "WARN", says: []string{"no cluster CA"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			var logs bytes.Buffer
			a := &App{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			cfg := pipelinesWiringConfig()
			cfg.CloneImage = tc.cloneImage
			apiBuilt, blobResolved := 0, 0
			runner := a.workbenchPipelineRunner(cfg, tc.inCluster,
				func() (*deploycontrol.ClusterAPI, error) {
					apiBuilt++
					if tc.apiErr != nil {
						return nil, tc.apiErr
					}
					return deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), nil
				},
				func() (server.FileUploader, string) {
					blobResolved++
					return &pipelinesFakeUploader{}, "memql"
				})

			if !tc.want {
				// The interface itself, not what it holds: a typed nil here
				// would be called by the handler rather than refused.
				if runner != nil {
					t.Fatalf("the node got a runner (%T) it cannot use", runner)
				}
				var said []string
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.Contains(line, "cannot run pipeline steps") {
						said = append(said, line)
					}
				}
				if len(said) != 1 {
					t.Fatalf("the node said why %d times, want once:\n%s", len(said), logs.String())
				}
				if !strings.Contains(said[0], "level="+tc.level) {
					t.Errorf("the line is not at %s:\n%s", tc.level, said[0])
				}
				for _, phrase := range tc.says {
					if !strings.Contains(said[0], phrase) {
						t.Errorf("the line does not name %q:\n%s", phrase, said[0])
					}
				}
				if blobResolved != 0 || (tc.apiErr == nil && apiBuilt != 0) {
					t.Errorf("a node that cannot run steps built %d API clients and %d blob clients", apiBuilt, blobResolved)
				}
				return
			}
			if _, ok := runner.(*pipelinesRunnerAdapter); !ok {
				t.Fatalf("the configured node got %T, want the runner behind its adapter", runner)
			}
			if apiBuilt != 1 || blobResolved != 1 {
				t.Errorf("the configured node built %d API clients and %d blob clients, want one of each", apiBuilt, blobResolved)
			}
			if strings.Contains(logs.String(), "cannot run pipeline steps") {
				t.Errorf("a configured node said it cannot run steps:\n%s", logs.String())
			}
		})
	}
}

// pipelinesForward sends one pipeline action through the handler, as the
// agent's executor does: under the engine's own SYSTEM assertion. The handler
// answers a pipeline action on a goroutine of its own, off the stream's
// receive loop, so its one reply is waited for.
func pipelinesForward(t *testing.T, h *workbench.ForwardHandler, action, args string) *nodev1.WorkbenchForwardResponse {
	t.Helper()
	assertion, err := auth.ForwardedAuthorityForSystem("pipelines:agent-a", time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForSystem: %v", err)
	}
	replies := make(chan *nodev1.NodeServerMessage, 4)
	h.HandleForwardedRequest(context.Background(), &nodev1.WorkbenchForwardRequest{
		RequestId: "req-" + action,
		RunId:     "run-1",
		Action:    action,
		ArgsJson:  []byte(args),
		Authority: node.ForwardedAuthorityToProto(assertion, "agent-a", "agent"),
	}, func(m *nodev1.NodeServerMessage) error {
		replies <- m
		return nil
	})
	var reply *nodev1.NodeServerMessage
	select {
	case reply = <-replies:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the handler never answered", action)
	}
	if reply.GetWorkbenchForwardResponse() == nil {
		t.Fatalf("%s: the handler answered %v, want a WorkbenchForwardResponse", action, reply)
	}
	return reply.GetWorkbenchForwardResponse()
}

// TestTheWorkbenchForwardHandlerAnswersThroughItsRunner. The handler a
// workbench node installs carries the node's runner: configured, a status
// forward reaches the REAL runner, which asks the API server in the
// configured namespace; unconfigured, it is answered pipelines_not_configured
// having asked nothing.
func TestTheWorkbenchForwardHandlerAnswersThroughItsRunner(t *testing.T) {
	srv, asked := pipelinesAPIServer(t)
	a := &App{Logger: quietLogger()}
	a.pipelineRunner = a.workbenchPipelineRunner(pipelinesWiringConfig(), true,
		func() (*deploycontrol.ClusterAPI, error) {
			return deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), nil
		},
		func() (server.FileUploader, string) { return nil, "" })
	if a.pipelineRunner == nil {
		t.Fatal("the configured node has no runner")
	}

	job := pipelinesteps.JobName("run-1", "tests.unit", 1)
	resp := pipelinesForward(t, a.workbenchForwardHandler(workbench.NewIntegration(quietLogger())),
		workbench.PipelineStatusAction, `{"jobName":"`+job+`"}`)
	if resp.GetErrorCode() != "" {
		t.Fatalf("a configured node refused the status forward: %s (%s)", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	var reply pipelinesteps.StatusReply
	if err := json.Unmarshal(resp.GetPayloadJson(), &reply); err != nil || reply.State != pipelinesteps.StateAbsent {
		t.Errorf("the status of a Job the API server does not have = %s (%v), want absent", resp.GetPayloadJson(), err)
	}
	want := "GET /apis/batch/v1/namespaces/steps-ns/jobs/" + job
	if got := asked(); len(got) != 1 || got[0] != want {
		t.Errorf("the API server was asked %v, want [%s]", got, want)
	}

	unconfigured := &App{Logger: quietLogger()}
	resp = pipelinesForward(t, unconfigured.workbenchForwardHandler(workbench.NewIntegration(quietLogger())),
		workbench.PipelineStatusAction, `{"jobName":"`+job+`"}`)
	if resp.GetErrorCode() != workbench.ErrCodePipelinesNotConfigured {
		t.Errorf("a node with no runner answered %q, want %q", resp.GetErrorCode(), workbench.ErrCodePipelinesNotConfigured)
	}
	if got := asked(); len(got) != 1 {
		t.Errorf("an unconfigured node reached the API server: %v", got)
	}
}

// parseAppFile parses one of this package's source files, whatever its build
// tag: the wiring below spans the workbench and agent builds, which no single
// compilation of this package sees together.
func parseAppFile(t *testing.T, fset *token.FileSet, name string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(fset, name, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

// selectorCalls is every call to <pkg>.<name> or <recv>.<name> in root.
func selectorCalls(root ast.Node, name string) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(root, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				out = append(out, call)
			}
		}
		return true
	})
	return out
}

// TestEveryPipelineRunnerFilesItsStepsInTheLibrary: the workbench's runner and
// the agent's fleet path are both handed pipelinesLibraryStoreFor's store, so
// a step's log and artifacts reach the owner's Library whichever surface ran
// it. Read from the source because the two constructions live in two builds
// (workbench and agent), and a nil Library -- what the fleet path had before
// this store existed -- compiles, runs every step, and files nothing.
func TestEveryPipelineRunnerFilesItsStepsInTheLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// The library argument's position in each constructor.
	libraryArg := map[string]int{"NewRunner": 3, "NewFleet": 2}
	found := map[string]int{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file := parseAppFile(t, fset, name)
		for ctor, at := range libraryArg {
			for _, call := range selectorCalls(file, ctor) {
				if pkg, ok := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident); !ok || pkg.Name != "pipelinesteps" {
					continue
				}
				found[ctor]++
				if len(call.Args) <= at {
					t.Fatalf("%s: pipelinesteps.%s takes %d arguments; re-read its signature", fset.Position(call.Pos()), ctor, len(call.Args))
				}
				arg, ok := call.Args[at].(*ast.CallExpr)
				sel, isSel := (*ast.SelectorExpr)(nil), false
				if ok {
					sel, isSel = arg.Fun.(*ast.SelectorExpr)
				}
				if !isSel || sel.Sel.Name != "pipelinesLibraryStoreFor" {
					t.Errorf("%s: pipelinesteps.%s is not handed pipelinesLibraryStoreFor's store, so its steps' files "+
						"would not reach the owner's Library the way the other surface's do", fset.Position(call.Pos()), ctor)
				}
			}
		}
	}
	// The reachable positive: one construction of each, or this test reads
	// a tree that no longer builds them here.
	for ctor := range libraryArg {
		if found[ctor] != 1 {
			t.Errorf("app/ constructs pipelinesteps.%s %d times, want once", ctor, found[ctor])
		}
	}
}

// TestTheWorkbenchNodeInstallsItsPipelineRunner pins the two links of the
// chain no single test can call: the workbench's integrations phase sets
// a.pipelineRunner from workbenchPipelineRunner, and the cluster phase builds
// the node's forward handler through workbenchForwardHandler -- the one place
// a handler is made, and the one that installs the runner. A workbench case
// that called workbench.NewForwardHandler itself would leave every pipeline
// action answering pipelines_not_configured on a node that can run them.
func TestTheWorkbenchNodeInstallsItsPipelineRunner(t *testing.T) {
	fset := token.NewFileSet()

	integrations := parseAppFile(t, fset, "integrations_workbench.go")
	assigned := false
	ast.Inspect(integrations, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok || lhs.Sel.Name != "pipelineRunner" {
			return true
		}
		if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "workbenchPipelineRunner" {
				assigned = true
			}
		}
		return true
	})
	if !assigned {
		t.Error("integrations_workbench.go never sets a.pipelineRunner from workbenchPipelineRunner, so no workbench node has a runner")
	}

	cluster := parseAppFile(t, fset, "cluster_workbench.go")
	var wire, build *ast.FuncDecl
	for _, decl := range cluster.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			switch fn.Name.Name {
			case "wireWorkbenchForwarding":
				wire = fn
			case "workbenchForwardHandler":
				build = fn
			}
		}
	}
	if wire == nil || build == nil {
		t.Fatal("cluster_workbench.go no longer declares wireWorkbenchForwarding and workbenchForwardHandler; move this test with them")
	}
	if n := len(selectorCalls(wire.Body, "workbenchForwardHandler")); n != 1 {
		t.Errorf("wireWorkbenchForwarding builds its handler through workbenchForwardHandler %d times, want once", n)
	}
	if n := len(selectorCalls(wire.Body, "NewForwardHandler")); n != 0 {
		t.Errorf("wireWorkbenchForwarding calls workbench.NewForwardHandler itself (%d times): that handler has no pipeline runner", n)
	}
	if n := len(selectorCalls(build.Body, "SetPipelineRunner")); n != 1 {
		t.Errorf("workbenchForwardHandler installs a pipeline runner %d times, want once", n)
	}
}
