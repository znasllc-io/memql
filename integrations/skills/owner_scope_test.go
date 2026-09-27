package skills

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// owner_scope_test.go -- runScript and captureScript act for their caller
// (memql.CallOwner). The owner routes a machine call to that person's fleet
// and names whose step a receipt is written onto, so a call naming another
// user is refused before the skill is read or anything is shipped, read back,
// run or written. The caller's own id and a trusted server-side context naming
// another user proceed -- the positive controls, without which a handler that
// refused everything would pass.

const (
	ownerA = "v1:identity:user:owner-a"
	ownerB = "v1:identity:user:owner-b"
)

func asPerson(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

// asAutomation is a shipped automation's context: the cluster's own synthetic
// actor under internal origin.
func asAutomation() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:ownerScopeTest", Role: auth.RoleReader, Synthetic: true, Unranked: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

func isOwnerRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), memql.OwnerNotCallerCode)
}

// ownerSeenSurface records the owner every call to the surface carried.
type ownerSeenSurface struct {
	Surface
	seen *[]string
}

func (s ownerSeenSurface) Call(ctx context.Context, req Request, action string, args map[string]any) (CallResult, error) {
	*s.seen = append(*s.seen, req.OwnerID)
	return s.Surface.Call(ctx, req, action, args)
}

// bindingOwners records whose step each binding was written under.
type bindingOwners []string

func (b *bindingOwners) StampBinding(_ context.Context, owner, _ string, _ map[string]any) error {
	*b = append(*b, owner)
	return nil
}

// ownerScopeFixture is the ordinary fixture with the machine surface watched.
func ownerScopeFixture(t *testing.T) (*Integration, *fakeSurface, *fakeArtifacts, *fakeSkills, *[]recordedCall, *[]string, *bindingOwners) {
	t.Helper()
	_, wb, fleet, sk, arts, calls := fixture(t)
	seen := &[]string{}
	bindings := &bindingOwners{}
	runner := NewRunner(sk, arts, wb, ownerSeenSurface{Surface: fleet, seen: seen}).WithBindings(bindings).WithLibrary(arts, sk)
	return NewIntegration(runner, nil), fleet, arts, sk, calls, seen, bindings
}

func machineArgs(owner string) map[string]any {
	return map[string]any{
		"skillId": "skill-1", "runId": "plan-1", "stepId": "step-1", "agentId": "agent-1",
		"ownerUserId":   owner,
		"requireLabels": map[string]any{"os": "darwin"},
	}
}

func onlyOwner(seen []string, want string) bool {
	if len(seen) == 0 {
		return false
	}
	for _, o := range seen {
		if o != want {
			return false
		}
	}
	return true
}

func TestRunScriptActsForItsCaller(t *testing.T) {
	integ, _, _, _, calls, _, bindings := ownerScopeFixture(t)
	if nodes, err := integ.handleRunScript(asPerson(ownerA), machineArgs(ownerB), 0); !isOwnerRefusal(err) || nodes != nil {
		t.Fatalf("a caller naming another user's id was answered: %v, %v", nodes, err)
	}
	if len(*calls) != 0 || len(*bindings) != 0 {
		t.Fatalf("the refused call reached a surface or wrote a binding: %d calls, bindings %v", len(*calls), *bindings)
	}

	for name, pc := range map[string]struct {
		ctx   context.Context
		owner string
	}{
		"the caller's own id":                 {asPerson(ownerA), ownerA},
		"internal origin naming another user": {asAutomation(), ownerB},
	} {
		integ, _, _, _, _, seen, bindings := ownerScopeFixture(t)
		nodes, err := integ.handleRunScript(pc.ctx, machineArgs(pc.owner), 0)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("%s: %v, %v", name, nodes, err)
		}
		var body map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &body); err != nil || body["ok"] != true {
			t.Fatalf("%s: the script did not run: %s", name, nodes[0].Payload)
		}
		if !onlyOwner(*seen, pc.owner) {
			t.Fatalf("%s: the machine was called for %v, want %s every time", name, *seen, pc.owner)
		}
		if len(*bindings) != 1 || (*bindings)[0] != pc.owner {
			t.Fatalf("%s: the binding was written under %v, want %s", name, *bindings, pc.owner)
		}
	}
}

func TestCaptureScriptActsForItsCaller(t *testing.T) {
	args := func(owner string) map[string]any {
		a := machineArgs(owner)
		a["path"], a["platform"], a["entry"] = "bin/reconcile.sh", "darwin", "bash {script}"
		return a
	}
	integ, fleet, arts, sk, calls, _, _ := ownerScopeFixture(t)
	fleet.files["bin/reconcile.sh"] = []byte("echo discovered\n")
	if nodes, err := integ.handleCaptureScript(asPerson(ownerA), args(ownerB), 0); !isOwnerRefusal(err) || nodes != nil {
		t.Fatalf("a caller naming another user's id was answered: %v, %v", nodes, err)
	}
	if len(*calls) != 0 || len(arts.wrote) != 0 || sk.written != nil {
		t.Fatalf("the refused call read the machine or filed something: %d calls, wrote %v, skills %v", len(*calls), arts.wrote, sk.written)
	}

	for name, pc := range map[string]struct {
		ctx   context.Context
		owner string
	}{
		"the caller's own id":                 {asPerson(ownerA), ownerA},
		"internal origin naming another user": {asAutomation(), ownerB},
	} {
		integ, fleet, arts, _, _, seen, _ := ownerScopeFixture(t)
		fleet.files["bin/reconcile.sh"] = []byte("echo discovered\n")
		nodes, err := integ.handleCaptureScript(pc.ctx, args(pc.owner), 0)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("%s: %v, %v", name, nodes, err)
		}
		if strings.Contains(string(nodes[0].Payload), `"ok":false`) || len(arts.wrote) != 1 {
			t.Fatalf("%s: nothing was captured: %s", name, nodes[0].Payload)
		}
		if !onlyOwner(*seen, pc.owner) {
			t.Fatalf("%s: the machine was read for %v, want %s every time", name, *seen, pc.owner)
		}
	}
}
