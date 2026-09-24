package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// userStorePartGrant is a grant source whose one grant is a USER-level allow
// of the store part.
type userStorePartGrant struct{}

func (userStorePartGrant) ActiveGrantsForUser(context.Context, string) ([]auth.Grant, error) {
	return []auth.Grant{{Verb: auth.VerbExecute, Resource: "app:deployables/store", Effect: auth.GrantAllow}}, nil
}

func (userStorePartGrant) ActiveGrantsForGroups(context.Context, []string) ([]auth.Grant, error) {
	return nil, nil
}

// THE DEPLOY'S PRE-CHECK ASKS BOTH STORE-PART CHECKS (Connect Shopify, D5;
// memql#5598). A binding change meets the caller's own store part AND the part
// at the site's organization, and the two can answer differently: the
// organization's answer admits only its members and keeps only its own groups.
// A pre-check asking only the first hands createSite a store the organization
// refuses, and the whole deploy is refused instead of placing the unattached
// draft. Here the caller holds the part, and acme does not admit them.
func TestMayChangeStoreBindingAsksTheSitesOrganization(t *testing.T) {
	oldGrant := auth.InstalledGrantSource()
	auth.SetGrantSource(userStorePartGrant{})
	t.Cleanup(func() { auth.SetGrantSource(oldGrant) })

	e := &MemQLEngine{}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "member", Role: auth.RoleWriter})
	const store = "v1:shopify:store:acme"

	create := map[string]any{"accountId": "acme", "binding": map[string]any{bindingStoreIdKey: store}}
	if e.validateOrganizationSensitiveChanges(ctx, conceptPlatformSite, nil, create) == nil {
		t.Fatal("fixture: the organization boundary must refuse this caller's binding at acme")
	}

	if e.MayChangeStoreBinding(ctx, "acme", "", store) {
		t.Error("the pre-check says yes to a binding the organization boundary refuses")
	}
	if e.MayChangeStoreBinding(ctx, "", "", store) {
		t.Error("a caller without a default organization cannot attach a store")
	}
}
