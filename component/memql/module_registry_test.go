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

func TestAuthorizeModuleRoles(t *testing.T) {
	unauthenticated := context.Background()
	if r := AuthorizeModuleRead(unauthenticated); r == nil || r.Code != moduleCodeUnauthenticated {
		t.Fatalf("unauthenticated read: got %+v", r)
	}
	if _, r := AuthorizeSetPackEnabled(unauthenticated); r == nil || r.Code != moduleCodeUnauthenticated {
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
	if r := AuthorizeModuleRead(asRole(auth.RoleAdmin)); r != nil {
		t.Errorf("admin must read the inventory: %+v", r)
	}
	if r := AuthorizeModuleRead(asRole(auth.RoleOwner)); r != nil {
		t.Errorf("owner must read the inventory: %+v", r)
	}

	if _, r := AuthorizeSetPackEnabled(asRole(auth.RoleAdmin)); r == nil || r.Code != moduleCodePermissionDenied {
		t.Errorf("admin must be refused the pack flip (owner-only): %+v", r)
	}
	actor, r := AuthorizeSetPackEnabled(asRole(auth.RoleOwner))
	if r != nil || actor.ID != "u-test" {
		t.Errorf("owner must pass the pack flip: actor=%+v refusal=%+v", actor, r)
	}
}

// testPackTree is a minimal in-memory pack tree: enough for domain
// registration; no loader ever walks it in these tests.
func testPackTree(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"concepts.memql": &fstest.MapFile{Data: []byte("// test pack tree\n")},
	}
}
