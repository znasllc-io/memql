package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestSiteOwnerStampSurvivesThePipelinesOwnStampedUpdate is the row-authz
// refusal a developer hits at Go live, reproduced at the seam that causes it.
//
// A first deploy runs createSite under the caller's actor, so the row is
// stamped with THEIR id. The pipeline then binds the site to its package with
// recordSitePackageOrigin, an update that names only packageId and
// packageDeployableName -- and sends it through store.writeInternal, which
// stamps INTERNAL ORIGIN onto a context that still carries the caller's
// AccessContext. That write is now "privileged" to applySiteOwnerStamp; the
// merged payload's ownerUserId is the caller's id (it is their row); the
// caller resolved from the context is the same id; so the undo written for
// "the value createSite just stamped" fires on a value nothing just stamped,
// deletes the owner, and the developer's site becomes cluster-owned. Their
// next write -- updateSiteStatus, live -- is refused as "not this caller's".
//
// The undo exists to keep a cluster owner or system actor from OWNING the
// site it creates. It must not fire on an update whose delta never named the
// owner at all.
func TestSiteOwnerStampSurvivesThePipelinesOwnStampedUpdate(t *testing.T) {
	const developer = "v1:identity:user:45c68303-d327-4d82-9923-ac47289f2c46"

	// The context the packages pipeline hands writeInternal: the deploying
	// developer's access context, with internal origin stamped for one write.
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: developer,
		Role:   auth.RoleDeveloper,
	}))

	// The MERGED payload of recordSitePackageOrigin: the stored row, owner
	// included, with the two fields the delta actually set.
	payload := map[string]any{
		"hostname":              "shop.memql.localhost",
		"kind":                  "static",
		"status":                "draft",
		"bundleRef":             "blob://sites/x/1/",
		"ownerUserId":           developer,
		"packageId":             "v1:platform:package:abc",
		"packageDeployableName": "storefront",
	}

	if err := applySiteOwnerStamp(ctx, payload, true /* priorExisted: an UPDATE */, "someone@example.com", false /* the delta named packageId + packageDeployableName, never the owner */); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	got, present := payload["ownerUserId"]
	if !present || got != developer {
		t.Fatalf("ownerUserId = %v (present=%v), want %q kept.\n"+
			"The pipeline's own stamped UPDATE just converted the developer's site into a "+
			"cluster-owned row; their Go-live click will now be refused as \"not this caller's\"",
			got, present, developer)
	}
}

// TestSiteOwnerStampStillUndoesACreateByADeploymentWriter is the control: the
// undo must keep firing on a CREATE by a privileged writer, or the seeded OS
// site and every cluster-owner-created site would come out owned by the actor
// that created them -- the state memql#4344 says a cluster owner cannot be in.
func TestSiteOwnerStampStillUndoesACreateByADeploymentWriter(t *testing.T) {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:seedMaterializer",
		Role:   auth.RoleOwner,
	}))
	payload := map[string]any{"hostname": "os.memql.localhost", "ownerUserId": "system:seedMaterializer"}
	if err := applySiteOwnerStamp(ctx, payload, false /* a CREATE */, "system:seedMaterializer", true); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	if _, present := payload["ownerUserId"]; present {
		t.Fatal("a CREATE by a deployment writer kept its owner stamp; the undo that makes such rows cluster-owned stopped firing")
	}
}

// TestSiteOwnerStampStillLetsAnOperatorTakeASiteBack keeps the documented
// hand-over: a cluster owner re-running createSite on an existing id arrives
// as an UPDATE whose delta DOES restate `ownerUserId: actor.userId`. That is a
// fresh stamp of the caller's own id, and it must still be undone so the row
// becomes the deployment's rather than the operator's.
func TestSiteOwnerStampStillLetsAnOperatorTakeASiteBack(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "root", Role: auth.RoleOwner})
	payload := map[string]any{"hostname": "shop.memql.localhost", "ownerUserId": "root"}
	if err := applySiteOwnerStamp(ctx, payload, true /* existing row */, "root", true /* createSite restated the owner */); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	if _, present := payload["ownerUserId"]; present {
		t.Fatal("an operator's createSite re-run kept ownerUserId = root; the take-back path stopped producing a cluster-owned row")
	}
}
