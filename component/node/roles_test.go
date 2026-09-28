package node

// roles_test.go -- the role table in roles.go is the node-type set, and every
// other spelling of it is held to it here (memql#5727).
//
// The readings, and why each one matters:
//
//   - ValidNodeTypes is derived from the table, so this is the guard against
//     someone re-typing it as a literal map again.
//   - app/build_<type>.go is what makes a build tag select a role; a role with
//     no build file builds as the untagged default and carries the wrong name.
//   - ENGINE_NODE_TYPES (scripts/lib/engine_build_args.sh) is the shell list
//     every image build and the k3d inner loop derive from. Compared IN ORDER,
//     because it is the order the table promises and the order the platform
//     graph and arch.yaml are written in.
//   - arch.yaml's roles block is how the base-tier architecture model learns
//     the role set without importing this package.
//
// scripts/ci/node_type_lists_test.go holds six more spellings (the deny-lists,
// compiled_<type>.go, the release and tag-pass matrices, the Deployments) to
// the build files, so together the two files tie every list to this table.
//
// These run under every node build tag (ci.yml's go-tests-tags lane tests this
// package once per tag), so nothing here may depend on which tag compiled it.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRootForRoles walks up from the package directory to the repository root,
// located by go.work -- since the module split this package is its own module,
// so a go.mod walk would stop here.
func repoRootForRoles(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work above %s", dir)
		}
		dir = parent
	}
}

func roleNames() []string {
	out := make([]string, 0, len(nodeRoles))
	for _, r := range Roles() {
		out = append(out, string(r.Type))
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestRolesAreWellFormed(t *testing.T) {
	seen := map[NodeType]bool{}
	for _, r := range Roles() {
		if r.Type == "" {
			t.Error("a role has no type")
		}
		if seen[r.Type] {
			t.Errorf("role %q is listed twice", r.Type)
		}
		seen[r.Type] = true
		if strings.TrimSpace(r.Description) == "" {
			t.Errorf("role %q has no description", r.Type)
		}
		got, ok := RoleFor(r.Type)
		if !ok || got != r {
			t.Errorf("RoleFor(%q) = %+v, %v; want the table's row", r.Type, got, ok)
		}
	}
	if _, ok := RoleFor("voice"); ok {
		t.Error("RoleFor answered for a retired node type")
	}
	if len(seen) != 7 {
		t.Errorf("the role table has %d roles, want 7; a role added or retired is a change to "+
			"every list this file holds to the table -- update them together, then this count", len(seen))
	}
}

// TestMeshRolesEqualValidNodeTypes: the mesh column IS ValidNodeTypes.
func TestMeshRolesEqualValidNodeTypes(t *testing.T) {
	var mesh []string
	for _, r := range Roles() {
		if r.Mesh {
			mesh = append(mesh, string(r.Type))
		}
	}
	var valid []string
	for nt, ok := range ValidNodeTypes {
		if ok {
			valid = append(valid, string(nt))
		}
	}
	if a, b := strings.Join(sortedCopy(mesh), ","), strings.Join(sortedCopy(valid), ","); a != b {
		t.Errorf("mesh roles %s != ValidNodeTypes %s", a, b)
	}
}

// TestIdentityAndEdgeAreTheNonMeshPair pins the two roles that exist but do
// not join the mesh. identity is the node-token issuer and has no node token to
// dial with; nothing dials an edge. Admitting either to the mesh set starts the
// worker dialer on it, tokenless (memql#5115).
func TestIdentityAndEdgeAreTheNonMeshPair(t *testing.T) {
	var nonMesh []string
	for _, r := range Roles() {
		if !r.Mesh {
			nonMesh = append(nonMesh, string(r.Type))
		}
	}
	if got := strings.Join(sortedCopy(nonMesh), ","); got != "edge,identity" {
		t.Errorf("non-mesh roles = %s, want edge,identity", got)
	}
}

// TestRolesMatchTheBuildFiles: one app/build_<type>.go per role, and no other.
func TestRolesMatchTheBuildFiles(t *testing.T) {
	root := repoRootForRoles(t)
	matches, err := filepath.Glob(filepath.Join(root, "app", "build_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, m := range matches {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "build_"), ".go")
		if base == "default" || strings.HasSuffix(base, "_test") {
			continue
		}
		files = append(files, base)
	}
	if len(files) == 0 {
		t.Fatal("no app/build_<type>.go files found -- the glob or the layout changed")
	}
	if a, b := strings.Join(sortedCopy(roleNames()), ","), strings.Join(sortedCopy(files), ","); a != b {
		t.Errorf("roles %s != app/build_<type>.go %s", a, b)
	}
}

var engineNodeTypesRe = regexp.MustCompile(`(?m)^ENGINE_NODE_TYPES=\(([^)]*)\)`)

// TestRolesMatchEngineNodeTypes: the shell list, in the table's order.
func TestRolesMatchEngineNodeTypes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootForRoles(t), "scripts", "lib", "engine_build_args.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := engineNodeTypesRe.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("scripts/lib/engine_build_args.sh has no literal ENGINE_NODE_TYPES=( ... ) assignment")
	}
	if a, b := strings.Join(roleNames(), " "), strings.Join(strings.Fields(m[1]), " "); a != b {
		t.Errorf("roles (%s) != ENGINE_NODE_TYPES (%s); same set, same order", a, b)
	}
}

// TestRolesMatchArchYAML: the architecture model's copy, field for field.
func TestRolesMatchArchYAML(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootForRoles(t), "arch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Roles []struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Mesh        bool   `yaml:"mesh"`
		} `yaml:"roles"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse arch.yaml: %v", err)
	}
	roles := Roles()
	if len(doc.Roles) != len(roles) {
		t.Fatalf("arch.yaml declares %d roles, roles.go %d", len(doc.Roles), len(roles))
	}
	for i, r := range roles {
		got := doc.Roles[i]
		if got.Name != string(r.Type) || got.Mesh != r.Mesh || got.Description != r.Description {
			t.Errorf("arch.yaml role %d = {%s mesh=%t %q}, roles.go = {%s mesh=%t %q}",
				i, got.Name, got.Mesh, got.Description, r.Type, r.Mesh, r.Description)
		}
	}
}

// TestRoutingRulesIsTheEffectiveTable: the exported table is the one
// evaluateRouting reads, so the platform graph records what routes.
func TestRoutingRulesIsTheEffectiveTable(t *testing.T) {
	got, want := RoutingRules(), defaultRoutingRules()
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("RoutingRules has %d rules, defaultRoutingRules %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	got[0].Pattern = "mutated"
	if RoutingRules()[0].Pattern == "mutated" {
		t.Error("RoutingRules returned the table itself; a caller could rewrite routing")
	}
}
