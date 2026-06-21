package deploycontrol

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestDeveloperGate covers the #1876 role matrix on the deploy-control
// surface:
//
//   - cut version + deploy (forward-deploy actions): allowed for
//     developer / admin / owner. SuggestNextVersion (the cut read
//     companion) shares that gate.
//   - rollback: stays owner/admin -- a developer can ship forward but
//     NOT roll back.
//   - writer / reader: read-only -- rejected on cut, deploy, AND
//     rollback.
func TestDeveloperGate(t *testing.T) {
	t.Run("developer may cut a version", func(t *testing.T) {
		eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{deploymentNode("production", "succeeded", "1.4.2")}}
		svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)

		res, err := svc.CutVersion(ctxWithRole(auth.RoleDeveloper), &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"})
		if err != nil {
			t.Fatalf("developer CutVersion: %v", err)
		}
		if !res.GetOk() {
			t.Fatalf("ok = false: %q", res.GetMessage())
		}
		if got := res.GetDetails()["version"]; got != "1.4.3" {
			t.Errorf("cut version = %q, want 1.4.3", got)
		}
	})

	t.Run("developer may suggest the next version", func(t *testing.T) {
		svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
		if _, err := svc.SuggestNextVersion(ctxWithRole(auth.RoleDeveloper), &memqlv1.SuggestNextVersionRequest{Env: "prod"}); err != nil {
			t.Fatalf("developer SuggestNextVersion: %v", err)
		}
	})

	t.Run("developer may deploy", func(t *testing.T) {
		eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
			fullDeploymentNode(map[string]any{
				"deploymentId": "d1", "status": "pending", "version": "1.2.3",
				"imageDigest": "sha256:abc", "provider": "azure", "environment": "staging",
			}),
		}}
		exec := &fakeExecutor{promoteOut: "SUCCESS: promoted"}
		svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

		res, err := svc.Deploy(ctxWithRole(auth.RoleDeveloper), &memqlv1.DeployRequest{DeploymentId: "d1"})
		if err != nil {
			t.Fatalf("developer Deploy: %v", err)
		}
		if !res.GetOk() {
			t.Fatalf("ok = false: %q", res.GetMessage())
		}
	})

	t.Run("admin may cut a version", func(t *testing.T) {
		eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{deploymentNode("production", "succeeded", "1.4.2")}}
		svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)

		res, err := svc.CutVersion(ctxWithRole(auth.RoleAdmin), &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"})
		if err != nil {
			t.Fatalf("admin CutVersion: %v", err)
		}
		if !res.GetOk() {
			t.Fatalf("ok = false: %q", res.GetMessage())
		}
	})

	// rollback is OWNER-ONLY (#1876): neither developer NOR admin may roll
	// back, even though both may cut + deploy forward.
	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleAdmin} {
		role := role
		t.Run(string(role)+" is REJECTED on rollback", func(t *testing.T) {
			eng := &fakeEngine{}
			audit := &fakeAudit{}
			svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, eng)

			_, err := svc.RollbackDeployment(ctxWithRole(role), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d1"})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("%s rollback code = %v, want PermissionDenied", role, status.Code(err))
			}
			// Gate fails closed: the engine is never touched on denial.
			if len(eng.queries) != 0 {
				t.Errorf("denied rollback must not touch the engine, got %v", eng.queries)
			}
			if len(audit.events) != 1 || audit.events[0].Outcome != "blocked" {
				t.Errorf("want one blocked audit event, got %+v", audit.events)
			}
		})
	}

	t.Run("owner may roll back", func(t *testing.T) {
		eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
			fullDeploymentNode(map[string]any{
				"deploymentId": "good1", "status": "succeeded", "version": "1.2.0",
				"imageDigest": "sha256:def", "provider": "azure", "environment": "staging",
			}),
		}}
		exec := &fakeExecutor{promoteOut: "SUCCESS: rolled back"}
		svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

		res, err := svc.RollbackDeployment(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "good1"})
		if err != nil {
			t.Fatalf("owner RollbackDeployment: %v", err)
		}
		if !res.GetOk() {
			t.Fatalf("ok = false: %q", res.GetMessage())
		}
	})

	// writer + reader are read-only: rejected on every write action,
	// including the forward-deploy ones.
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		role := role
		t.Run("read-only role "+string(role)+" rejected on cut/deploy/rollback", func(t *testing.T) {
			svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})

			if _, err := svc.CutVersion(ctxWithRole(role), &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s CutVersion code = %v, want PermissionDenied", role, status.Code(err))
			}
			if _, err := svc.SuggestNextVersion(ctxWithRole(role), &memqlv1.SuggestNextVersionRequest{Env: "prod"}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s SuggestNextVersion code = %v, want PermissionDenied", role, status.Code(err))
			}
			if _, err := svc.Deploy(ctxWithRole(role), &memqlv1.DeployRequest{DeploymentId: "d1"}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s Deploy code = %v, want PermissionDenied", role, status.Code(err))
			}
			if _, err := svc.RollbackDeployment(ctxWithRole(role), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d1"}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s RollbackDeployment code = %v, want PermissionDenied", role, status.Code(err))
			}
		})
	}
}
