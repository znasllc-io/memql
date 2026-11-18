package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE CALLS SWITCH ASKS ITS OWN PART WHEN A SITE IS CREATED, NOT ONLY WHEN IT CHANGES.
// A site tier admits writes from account members, and validateOrganizationSensitiveChanges is
// what judges a raw upsert. siteFieldParts is applied only on an existing site, so a create
// naming `calls: true` asked for no part at all and publishing it later asked only publish,
// which would defeat the separate app:deployables/calls part. A create that leaves the switch
// off is ordinary data and must stay unjudged.
func TestCreatingASiteWithCallsOnAsksTheCallsPart(t *testing.T) {
	e := &MemQLEngine{}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "member", Role: auth.RoleWriter})

	err := e.validateOrganizationSensitiveChanges(ctx, conceptPlatformSite, nil, map[string]any{"accountId": "acme", "calls": true})
	if err == nil {
		t.Fatal("a site created with calls on asked no part: a member with no calls part switched the capability on")
	}
	if !strings.Contains(err.Error(), CodeCapabilityNotHeld) || !strings.Contains(err.Error(), "calls") {
		t.Fatalf("refusal = %v, want %s naming the calls part", err, CodeCapabilityNotHeld)
	}

	if err := e.validateOrganizationSensitiveChanges(ctx, conceptPlatformSite, nil, map[string]any{"accountId": "acme", "calls": false}); err != nil {
		t.Errorf("a create that leaves calls off is ordinary data, got %v", err)
	}
	if err := e.validateOrganizationSensitiveChanges(ctx, conceptPlatformSite, nil, map[string]any{"accountId": "acme"}); err != nil {
		t.Errorf("a create that names no calls switch is ordinary data, got %v", err)
	}

	prior := map[string]any{"accountId": "acme"}
	if err := e.validateOrganizationSensitiveChanges(ctx, conceptPlatformSite, prior, map[string]any{"accountId": "acme", "calls": true}); err == nil {
		t.Error("switching calls on for an existing site asked no part")
	}
}
