package deploypack_test

// deploy_pack_test.go proves the deploy pack (Epic 2 / #2095) loads into the
// engine registries alongside core and exposes its four deploy effects as Go
// capabilities, with NO database and NO real cluster. It runs under the default
// `go test ./...` (no build tag), mirroring examples/referencepack.
//
// Coverage (exported-API-provable half of the acceptance):
//   - the pack's IntegrationProvider exposes all four deploy capabilities under
//     the FQNs the dsl/builtins.memql @executors name.
//   - each capability handler routes through the deploycontrol Executor (the
//     SAME side-effect boundary the Deploy Console uses): runPromote calls
//     RunPromote, commitOverlay/argoSync call Git, recordBack calls the E2.1
//     deployment mutations through the engine. The fake Executor + engine prove
//     the wiring WITHOUT a real promote.sh / git / cluster -- the azure path's
//     RunPromote contract is exercised, not bypassed.
//   - the contract-version gate accepts the pack's pinned ContractVersion.
//
// The builtin-registration half (the pack's builtins resolving through
// LoadUnifiedBuiltins) lives in component/memql/deploy_pack_load_test.go (needs
// the unexported registry constructor). Together they prove load + extend.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	deploypack "github.com/znasllc-io/memql/examples/deploypack"
)

// errBoom is a sentinel error a fake Executor returns to simulate a promote
// failure the lifecycle automation must observe in-band.
var errBoom = errors.New("boom")

// fakeExecutor records the deploycontrol Executor calls a handler makes so a
// test can assert the effect routed through the sanctioned boundary. Satisfies
// deploycontrol.Executor.
type fakeExecutor struct {
	promoteVersion string
	promoteEnv     string
	promoteErr     error // when set, RunPromote returns it (simulates a promote failure)
	gitCalls       [][]string
}

func (f *fakeExecutor) RunPromote(_ context.Context, version, env string) (string, error) {
	f.promoteVersion = version
	f.promoteEnv = env
	if f.promoteErr != nil {
		return "promote failed", f.promoteErr
	}
	return "promoted " + version + " to " + env, nil
}
func (f *fakeExecutor) RunRollback(_ context.Context, _, _ string) (string, error) { return "", nil }
func (f *fakeExecutor) RunRolloutAction(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeExecutor) KubectlJSON(_ context.Context, _ ...string) ([]byte, error) { return nil, nil }
func (f *fakeExecutor) Git(_ context.Context, args ...string) (string, error) {
	f.gitCalls = append(f.gitCalls, args)
	return "git " + strings.Join(args, " "), nil
}

// fakeEngine records the DSL mutations recordBack runs.
type fakeEngine struct{ queries []string }

func (f *fakeEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, query)
	return &memqlengine.ExecuteResult{}, nil
}

func TestDeployPackProviderExposesAllEffects(t *testing.T) {
	provider, err := deploypack.NewProvider(memqlengine.PluginContext{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := provider.IntegrationName(); got != "deploypack" {
		t.Fatalf("IntegrationName() = %q, want deploypack", got)
	}
	want := map[string]bool{
		"commitOverlay": false,
		"argoSync":      false,
		"runPromote":    false,
		"recordBack":    false,
	}
	for _, c := range provider.Capabilities() {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
			if c.Handler == nil {
				t.Errorf("capability %q has a nil Handler -- the @executor would dangle", c.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("provider Capabilities() MUST include %q so integration.deploypack.%s resolves", name, name)
		}
	}
}

// TestDeployPackRunPromoteUsesExecutor is the azure-path-preservation proof:
// the runPromote effect calls the deploycontrol Executor's RunPromote with the
// version + env -- the SAME contract the live Deploy Console path uses.
func TestDeployPackRunPromoteUsesExecutor(t *testing.T) {
	exec := &fakeExecutor{}
	provider := deploypack.NewProviderWithDeps(exec, &fakeEngine{})

	if _, err := callCapability(t, provider, "runPromote",
		map[string]any{"version": "2026.6.21", "env": "staging"}); err != nil {
		t.Fatalf("runPromote handler: %v", err)
	}
	if exec.promoteVersion != "2026.6.21" || exec.promoteEnv != "staging" {
		t.Fatalf("runPromote called RunPromote(version=%q, env=%q), want (2026.6.21, staging)",
			exec.promoteVersion, exec.promoteEnv)
	}
}

// TestDeployPackRunPromoteNormalizesEnv proves the runPromote effect maps the
// deployment.environment enum to the promote.sh console env the same way the Go
// driver does (production -> prod), so the E2.3 lifecycle automation can pass
// the raw enum through. The env mapping lives in ONE place (ConsoleEnvFor).
func TestDeployPackRunPromoteNormalizesEnv(t *testing.T) {
	exec := &fakeExecutor{}
	provider := deploypack.NewProviderWithDeps(exec, &fakeEngine{})

	if _, err := callCapability(t, provider, "runPromote",
		map[string]any{"version": "1.2.3", "env": "production"}); err != nil {
		t.Fatalf("runPromote handler: %v", err)
	}
	if exec.promoteEnv != "prod" {
		t.Fatalf("runPromote(env=production) -> RunPromote env=%q, want prod (ConsoleEnvFor mapping)", exec.promoteEnv)
	}
}

// TestDeployPackRunPromoteReportsOutcomeInBand is the E2.3 (#2096) contract the
// lifecycle automation relies on: a promote FAILURE is reported as success=false
// in the result node (NOT a Go error), so the automation can branch to a failed
// transition rather than aborting the step. A clean promote reports success=true.
func TestDeployPackRunPromoteReportsOutcomeInBand(t *testing.T) {
	// Clean promote -> success=true.
	okProvider := deploypack.NewProviderWithDeps(&fakeExecutor{}, &fakeEngine{})
	nodes, err := callCapability(t, okProvider, "runPromote", map[string]any{"version": "1.0.0", "env": "staging"})
	if err != nil {
		t.Fatalf("runPromote (clean) returned a Go error, want in-band success: %v", err)
	}
	if got := successOf(t, nodes); got != true {
		t.Fatalf("clean promote success=%v, want true", got)
	}

	// Failed promote -> success=false, NO Go error (the lifecycle must see it).
	failProvider := deploypack.NewProviderWithDeps(&fakeExecutor{promoteErr: errBoom}, &fakeEngine{})
	nodes, err = callCapability(t, failProvider, "runPromote", map[string]any{"version": "1.0.0", "env": "staging"})
	if err != nil {
		t.Fatalf("runPromote (failed) MUST NOT return a Go error -- it would abort the "+
			"lifecycle automation before the failed transition: %v", err)
	}
	if got := successOf(t, nodes); got != false {
		t.Fatalf("failed promote success=%v, want false", got)
	}
}

func TestDeployPackCommitAndSyncUseGit(t *testing.T) {
	exec := &fakeExecutor{}
	provider := deploypack.NewProviderWithDeps(exec, &fakeEngine{})

	if _, err := callCapability(t, provider, "commitOverlay", map[string]any{"env": "prod"}); err != nil {
		t.Fatalf("commitOverlay handler: %v", err)
	}
	if _, err := callCapability(t, provider, "argoSync", map[string]any{"env": "prod"}); err != nil {
		t.Fatalf("argoSync handler: %v", err)
	}
	// commitOverlay -> git add + git commit; argoSync -> git push.
	var sawAdd, sawCommit, sawPush bool
	for _, c := range exec.gitCalls {
		switch c[0] {
		case "add":
			sawAdd = true
		case "commit":
			sawCommit = true
		case "push":
			sawPush = true
		}
	}
	if !sawAdd || !sawCommit {
		t.Errorf("commitOverlay must git add + commit the overlay; calls=%v", exec.gitCalls)
	}
	if !sawPush {
		t.Errorf("argoSync must git push so ArgoCD reconciles; calls=%v", exec.gitCalls)
	}
}

// TestDeployPackRecordBackRunsMutations proves the Model A record-back effect
// appends a status transition + a per-node spec through the E2.1 mutations.
func TestDeployPackRecordBackRunsMutations(t *testing.T) {
	eng := &fakeEngine{}
	provider := deploypack.NewProviderWithDeps(&fakeExecutor{}, eng)

	_, err := callCapability(t, provider, "recordBack", map[string]any{
		"deploymentId": "dep-1",
		"status":       "succeeded",
		"nodeType":     "bff",
		"version":      "",
		"replicas":     2,
		"imageDigest":  "sha256:abc",
	})
	if err != nil {
		t.Fatalf("recordBack handler: %v", err)
	}
	joined := strings.Join(eng.queries, "\n")
	if !strings.Contains(joined, "updateDeploymentStatus(") {
		t.Errorf("recordBack must run updateDeploymentStatus; queries=%v", eng.queries)
	}
	if !strings.Contains(joined, "createDeploymentNodeSpec(") {
		t.Errorf("recordBack must run createDeploymentNodeSpec for the per-node spec; queries=%v", eng.queries)
	}
	if !strings.Contains(joined, `nodeType: "bff"`) || !strings.Contains(joined, "replicas: 2") {
		t.Errorf("recordBack nodeSpec call missing nodeType/replicas; queries=%v", eng.queries)
	}
}

func TestDeployPackContractCompat(t *testing.T) {
	if err := memqlengine.CheckPluginContractCompat(deploypack.ContractVersion); err != nil {
		t.Fatalf("pack ContractVersion %d should be compatible with core %d: %v",
			deploypack.ContractVersion, memqlengine.PluginContractVersion, err)
	}
	reg := memqlengine.PluginRegistration{
		Name:                    deploypack.Domain,
		RequiresContractVersion: deploypack.ContractVersion,
	}
	if err := reg.ValidateContract(); err != nil {
		t.Fatalf("PluginRegistration.ValidateContract for the pack should pass: %v", err)
	}
}

// callCapability resolves a capability's handler by name and invokes it,
// failing the test if the capability or its handler is missing.
func callCapability(t *testing.T, provider memqlengine.IntegrationProvider, name string, args map[string]any) ([]memorynodes.MemoryNode, error) {
	t.Helper()
	for _, c := range provider.Capabilities() {
		if c.Name == name {
			if c.Handler == nil {
				t.Fatalf("capability %q has a nil handler", name)
			}
			return c.Handler(context.Background(), args, 0)
		}
	}
	t.Fatalf("capability %q not found", name)
	return nil, nil
}

// successOf reads the `success` field off the single result node a deploy
// effect returns -- the in-band outcome the E2.3 lifecycle automation branches
// on (result.First().payload.success).
func successOf(t *testing.T, nodes []memorynodes.MemoryNode) bool {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 result node, got %d", len(nodes))
	}
	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal result payload: %v", err)
	}
	return payload.Success
}
