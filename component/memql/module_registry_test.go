package memql

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/auth"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestModuleEnvSurfaceNeverCarriesASecretValue is the structural half of the
// design's "secrets never leave the engine, in any form" rule (section 3):
// plant a sentinel in a manifest-listed secret's env var and assert the
// whole env surface -- every component, every entry -- never carries it, and
// that no secret entry carries ANY value, masked or otherwise.
func TestModuleEnvSurfaceNeverCarriesASecretValue(t *testing.T) {
	manifest, err := moduleManifest()
	if err != nil {
		t.Fatalf("manifest load: %v", err)
	}
	if len(manifest.Secrets) == 0 {
		t.Fatalf("manifest has no secrets; the assertion would be vacuous")
	}

	const sentinel = "sk-THIS-VALUE-MUST-NEVER-CROSS-THE-WIRE"
	target := manifest.Secrets[0].Name
	t.Setenv(target, sentinel)

	components := map[string]struct{}{}
	for _, entry := range manifest.AllEntries() {
		if c := strings.TrimSpace(entry.Component); c != "" {
			components[c] = struct{}{}
		}
	}
	sawTarget := false
	for comp := range components {
		for _, v := range moduleEnvSurface(manifest, []string{comp}) {
			if v.Secret && (v.Value != "" || v.DefaultValue != "") {
				t.Errorf("secret env var %s (component %s) carries a value on the surface", v.Name, comp)
			}
			if strings.Contains(v.Value, sentinel) || strings.Contains(v.DefaultValue, sentinel) {
				t.Errorf("sentinel secret value leaked through env var %s (component %s)", v.Name, comp)
			}
			if v.Name == target {
				sawTarget = true
				if !v.Set {
					t.Errorf("secret %s is set in env but reported unset -- set/unset is the one fact the surface owes", target)
				}
			}
		}
	}
	if !sawTarget {
		// The reachable-positive rule: prove the instrument could have moved.
		t.Fatalf("target secret %s never appeared on any component surface; assertion did not exercise the leak path", target)
	}
}

func TestComponentModuleRows(t *testing.T) {
	manifest, err := moduleManifest()
	if err != nil {
		t.Fatalf("manifest load: %v", err)
	}
	rows := componentModuleRows(manifest)
	if len(rows) == 0 {
		t.Fatalf("manifest yielded zero component modules")
	}
	seen := map[string]struct{}{}
	for _, r := range rows {
		if r.Kind != ModuleKindComponent || r.State != "built_in" || r.Scope != ModuleScopeNode {
			t.Errorf("component row %q has wrong kind/state/scope: %+v", r.Name, r)
		}
		if _, dup := seen[r.Name]; dup {
			t.Errorf("duplicate component row %q", r.Name)
		}
		seen[r.Name] = struct{}{}
	}
}

func TestPackModuleRowsStateAndHonesty(t *testing.T) {
	const domain = "modregtestpack"
	memqldsl.RegisterTree(domain, testPackTree(t))
	t.Cleanup(func() {
		memqldsl.UnregisterTree(domain)
		memqldsl.SetDisabledPackDomains(nil)
		unbindPluginFromPackForTest("modregtestplugin")
	})
	BindPluginToPack("modregtestplugin", domain)

	// Desired state (graph) says disabled; this node booted BEFORE the flip
	// (loaders' set empty) -- the row must say disabled AND surface the
	// restart-required disagreement.
	states := map[string]PackStateRow{
		domain: {PackDomain: domain, Enabled: false, Reason: "maintenance"},
	}
	rows, bound := packModuleRows(states)

	var row *ModuleRow
	for i := range rows {
		if rows[i].Name == domain {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("registered pack domain %q missing from pack rows: %+v", domain, rows)
	}
	if row.State != "disabled" || row.Scope != ModuleScopeCluster {
		t.Errorf("pack row state/scope = %q/%q; want disabled/cluster", row.State, row.Scope)
	}
	if !strings.Contains(row.StateDetail, "restart required") {
		t.Errorf("boot-vs-desired disagreement not surfaced: %q", row.StateDetail)
	}
	if !strings.Contains(row.StateDetail, "maintenance") {
		t.Errorf("operator reason not surfaced: %q", row.StateDetail)
	}
	if len(row.FqnPrefixes) != 1 || row.FqnPrefixes[0] != "integration.modregtestplugin." {
		t.Errorf("pack fqn prefixes = %v", row.FqnPrefixes)
	}
	if _, ok := bound["modregtestplugin"]; !ok {
		t.Errorf("bound plugin set missing modregtestplugin")
	}

	// Now align the node with the desired state: mounted-inert here too.
	memqldsl.SetDisabledPackDomains([]string{domain})
	rows, _ = packModuleRows(states)
	for _, r := range rows {
		if r.Name == domain {
			if strings.Contains(r.StateDetail, "restart required") {
				t.Errorf("aligned state still reports restart required: %q", r.StateDetail)
			}
			if !strings.Contains(r.StateDetail, "mounted-inert") {
				t.Errorf("inert boot outcome not surfaced: %q", r.StateDetail)
			}
		}
	}
}

// testStorefrontPack declares a storefront pack for one test. referencepack is
// the not-flippable fixture: it is a real pack that never declares itself one.
func testStorefrontPack(t *testing.T, domain string) {
	t.Helper()
	memqldsl.RegisterStorefrontPack(domain)
	t.Cleanup(func() { memqldsl.UnregisterStorefrontPack(domain) })
}

func TestAuthorizeModuleRoles(t *testing.T) {
	const storefront = "modregtest-storefront"
	const other = "referencepack"
	testStorefrontPack(t, storefront)

	unauthenticated := context.Background()
	if r := AuthorizeModuleRead(unauthenticated); r == nil || r.Code != moduleCodeUnauthenticated {
		t.Fatalf("unauthenticated read: got %+v", r)
	}
	if _, r := AuthorizeSetPackEnabled(unauthenticated, storefront); r == nil || r.Code != moduleCodeUnauthenticated {
		t.Fatalf("unauthenticated write: got %+v", r)
	}

	asRole := func(role auth.Role) context.Context {
		return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "u-test", PrimaryEmail: "t@example.com", Role: role,
		})
	}

	if r := AuthorizeModuleRead(asRole(auth.RoleReader)); r == nil || r.Code != moduleCodePermissionDenied {
		t.Errorf("reader must be refused the inventory: %+v", r)
	}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOwner, auth.RoleDeveloper} {
		if r := AuthorizeModuleRead(asRole(role)); r != nil {
			t.Errorf("%s must read the inventory: %+v", role, r)
		}
	}

	// An owner flips any pack, storefront or not, as before.
	for _, pack := range []string{storefront, other} {
		actor, r := AuthorizeSetPackEnabled(asRole(auth.RoleOwner), pack)
		if r != nil || actor.ID != "u-test" {
			t.Errorf("owner must pass the %s flip: actor=%+v refusal=%+v", pack, actor, r)
		}
	}

	// A developer flips a storefront pack and nothing else.
	if actor, r := AuthorizeSetPackEnabled(asRole(auth.RoleDeveloper), storefront); r != nil || actor.ID != "u-test" {
		t.Errorf("developer must pass the storefront flip: actor=%+v refusal=%+v", actor, r)
	}
	if _, r := AuthorizeSetPackEnabled(asRole(auth.RoleDeveloper), other); r == nil || r.Code != moduleCodePermissionDenied {
		t.Errorf("developer must be refused a pack that is not a storefront pack: %+v", r)
	}

	// Admin holds no flip at all, storefront included: reading the inventory
	// is not managing it.
	for _, pack := range []string{storefront, other} {
		if _, r := AuthorizeSetPackEnabled(asRole(auth.RoleAdmin), pack); r == nil || r.Code != moduleCodePermissionDenied {
			t.Errorf("admin must be refused the %s flip: %+v", pack, r)
		}
	}
	if _, r := AuthorizeSetPackEnabled(asRole(auth.RoleReader), storefront); r == nil || r.Code != moduleCodePermissionDenied {
		t.Errorf("reader must be refused the storefront flip: %+v", r)
	}
}

// TestListModulesReportsMayFlipPerCaller: the switch the OS draws is the
// answer AuthorizeSetPackEnabled would give, per pack row, and only a pack row
// can carry it.
func TestListModulesReportsMayFlipPerCaller(t *testing.T) {
	const storefront = "modregtest-flipstore"
	const plain = "modregtest-flipplain"
	for _, d := range []string{storefront, plain} {
		memqldsl.RegisterTree(d, testPackTree(t))
		d := d
		t.Cleanup(func() { memqldsl.UnregisterTree(d) })
	}
	testStorefrontPack(t, storefront)

	want := map[auth.Role]map[string]bool{
		auth.RoleOwner:     {storefront: true, plain: true},
		auth.RoleDeveloper: {storefront: true, plain: false},
		auth.RoleAdmin:     {storefront: false, plain: false},
	}
	e := &MemQLEngine{}
	for role, packs := range want {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-flip", Role: role})
		rows, err := e.ListModules(ctx)
		if err != nil {
			t.Fatalf("ListModules as %s: %v", role, err)
		}
		seen := 0
		for _, row := range rows {
			if row.Kind != ModuleKindPack {
				if row.MayFlip {
					t.Errorf("%s %q carries mayFlip; only a pack has a switch", row.Kind, row.Name)
				}
				continue
			}
			if expect, ok := packs[row.Name]; ok {
				seen++
				if row.MayFlip != expect {
					t.Errorf("as %s, pack %q mayFlip = %v, want %v", role, row.Name, row.MayFlip, expect)
				}
			}
		}
		if seen != len(packs) {
			t.Fatalf("as %s, saw %d of the %d test packs in the inventory", role, seen, len(packs))
		}
	}
}

// testPackTree is a minimal in-memory pack tree: enough for domain
// registration; no loader ever walks it in these tests.
func testPackTree(t *testing.T) fstest.MapFS {
	t.Helper()
	return withLanguageLine(fstest.MapFS{
		"concepts.memql": &fstest.MapFile{Data: []byte("// test pack tree\n")},
	})
}
