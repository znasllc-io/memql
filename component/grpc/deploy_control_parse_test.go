package memql

// deploy_control_parse_test.go runs every query string the deploy-control
// verbs issue through the REAL MemQL parser, with no database.
//
// WHY THIS EXISTS. component/deploycontrol's own suite drives a fake engine
// that records query strings and parses nothing. That is the failure mode
// voice_agent_real_engine_test.go describes for a different handler, and it
// hid the same defect here: every record read and write in the package was
// rendered as the legacy `name({k: v})` object-literal wrapper, which the
// parser has rejected since Story 9 of memql#2335 -- so CutVersion, Deploy,
// RollbackDeployment and every status transition had been failing at parse in
// production while the unit suite stayed green. memql#4209's end-to-end test
// on a live engine (deploy_control_repair_db_test.go) found it; this file is
// the DB-free guard that keeps it found. It lives in this package because
// deploycontrol cannot boot an engine without widening its module, and this
// package already boots the real embedded-DSL engine for exactly this purpose
// (newRealDSLEngine).
//
// It drives the REAL service over the forward hop, so what is parsed is what
// the shipped code renders, not a restatement of it.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// parseCheckingEngine runs every query through the real parser before
// answering it from a canned bundle. A string the parser rejects surfaces as
// an Execute error -- exactly as it does in production -- and is recorded.
type parseCheckingEngine struct {
	eng *memqlengine.MemQLEngine
	mu  sync.Mutex
	// parsed and rejected are the queries the real parser accepted / refused.
	parsed   []string
	rejected []string
	// nodes is the canned read result: one row shaped like a succeeded
	// deployment that also carries a cluster provider, so every verb's read
	// leg finds what it needs and reaches its write leg.
	nodes []*memqlv1.MemoryNode
}

func (p *parseCheckingEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.eng.Parse(query); err != nil {
		p.rejected = append(p.rejected, query+"  -->  "+err.Error())
		return nil, err
	}
	p.parsed = append(p.parsed, query)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: p.nodes}}, nil
}

func (p *parseCheckingEngine) snapshot() (parsed, rejected []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.parsed...), append([]string(nil), p.rejected...)
}

func (p *parseCheckingEngine) sawPrefix(prefix string) bool {
	parsed, _ := p.snapshot()
	for _, q := range parsed {
		if strings.HasPrefix(q, prefix) {
			return true
		}
	}
	return false
}

// permissiveExecutor lets every effect succeed and reports the Application
// as already synced and healthy under whatever operation was last stamped,
// so a repair's watcher resolves on its first observation.
type permissiveExecutor struct {
	mu     sync.Mutex
	marker string
}

func (e *permissiveExecutor) RunRollback(context.Context, string) (string, error) { return "ok", nil }
func (e *permissiveExecutor) RunRolloutAction(context.Context, string, string) (string, error) {
	return "ok", nil
}
func (e *permissiveExecutor) RunRepair(_ context.Context, marker string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.marker = marker
	return "ok", nil
}
func (e *permissiveExecutor) Git(context.Context, ...string) (string, error) { return "", nil }
func (e *permissiveExecutor) KubectlJSON(context.Context, ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return []byte(`{"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},` +
		`"operationState":{"phase":"Succeeded","operation":{"info":[{"name":"memql.io/repair","value":"` + e.marker + `"}]}}}}`), nil
}

func TestDeployControlQueriesParseOnTheRealEngine(t *testing.T) {
	real := newRealDSLEngine(t)
	fields, err := structpb.NewStruct(map[string]any{
		"deploymentId": "d-parse", "status": "succeeded", "version": "1.2.0",
		"imageDigest": "sha256:abc", "provider": "azure", "name": "memql",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := &parseCheckingEngine{eng: real, nodes: []*memqlv1.MemoryNode{{Payload: fields}}}

	svc, err := deploycontrol.NewService(deploycontrol.Options{
		Logger:             testLogger(),
		Audit:              &recordingAudit{},
		RepoRoot:           t.TempDir(),
		Executor:           &permissiveExecutor{},
		Engine:             engine,
		RepairPollInterval: time.Millisecond,
		RepairCeiling:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewDeployControlForwardHandler(svc, testLogger())

	// Every verb that touches the engine, driven as the owner over the hop.
	// Each must come back ok at BOTH levels: the hop, and the action -- an
	// action whose query failed to parse answers ok=false with the parser's
	// message, which is the signal this test exists to surface.
	verbs := []struct {
		name string
		msg  *memqlv1.DeployControlMsg
	}{
		{"CutVersion", &memqlv1.DeployControlMsg{RequestId: "p-cut", Request: &memqlv1.DeployControlMsg_CutVersion{
			CutVersion: &memqlv1.CutVersionRequest{Bump: "patch"}}}},
		{"Deploy", &memqlv1.DeployControlMsg{RequestId: "p-deploy", Request: &memqlv1.DeployControlMsg_Deploy{
			Deploy: &memqlv1.DeployRequest{DeploymentId: "d-parse"}}}},
		{"RollbackDeployment", &memqlv1.DeployControlMsg{RequestId: "p-rollback", Request: &memqlv1.DeployControlMsg_RollbackDeployment{
			RollbackDeployment: &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d-parse"}}}},
		{"Repair", &memqlv1.DeployControlMsg{RequestId: "p-repair", Request: &memqlv1.DeployControlMsg_Repair{
			Repair: &memqlv1.RepairRequest{}}}},
		{"SuggestNextVersion", &memqlv1.DeployControlMsg{RequestId: "p-suggest", Request: &memqlv1.DeployControlMsg_SuggestNextVersion{
			SuggestNextVersion: &memqlv1.SuggestNextVersionRequest{}}}},
	}
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			res, err := runHop(t, h, principalForRole(t, auth.RoleOwner), v.msg)
			if err != nil {
				t.Fatalf("hop: %v", err)
			}
			if !res.GetOk() {
				t.Fatalf("%s refused: code=%v %s", v.name, codes.Code(res.GetErrorCode()), res.GetErrorMessage())
			}
			if action := res.GetAction(); action != nil && !action.GetOk() {
				t.Fatalf("%s ran and failed: %s", v.name, action.GetMessage())
			}
		})
	}

	// The repair watcher's terminal write runs after the RPC answered; wait
	// for it so its updateDeploymentStatus string is parse-checked too (Deploy
	// wrote the first transition synchronously, the watcher writes the second).
	deadline := time.Now().Add(5 * time.Second)
	for countPrefix(engine, "updateDeploymentStatus(") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	parsed, rejected := engine.snapshot()
	if len(rejected) != 0 {
		t.Fatalf("the real parser rejected %d deploy-control query string(s):\n  %s",
			len(rejected), strings.Join(rejected, "\n  "))
	}
	// The guard must have seen every kind of string the package renders, or
	// it is green for the wrong reason.
	for _, prefix := range []string{
		"deploymentById(", "deploymentsForCluster(", "existingCluster(",
		"createDeployment(", "updateDeploymentStatus(",
	} {
		if !engine.sawPrefix(prefix) {
			t.Errorf("no %s... query reached the parser; parsed = %v", prefix, parsed)
		}
	}
}

func countPrefix(engine *parseCheckingEngine, prefix string) int {
	parsed, _ := engine.snapshot()
	n := 0
	for _, q := range parsed {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}
