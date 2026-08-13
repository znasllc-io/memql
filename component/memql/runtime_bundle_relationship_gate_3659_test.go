package memql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// This file is the answer to memql#3629 for relationships.
//
// Every relationship gate this epic added is enforced inside the ENGINE'S LOAD
// PATH, but until now every TEST of those gates fed the engine concepts built
// in Go. That proves the gate works; it does not prove it fires for a PRODUCT
// DSL BUNDLE -- a directory of .memql files mounted at MEMQL_DSL_PATH by a repo
// that cannot patch this engine. Those bundles are the primary consumer of the
// DSL surface and the only consumer with no Go test of its own.
//
// The harness below writes a synthetic bundle to a temp dir, mounts it exactly
// as app/database.go does at boot, and asserts the engine REFUSES TO LOAD. It
// is deliberately generic: any future DSL construct gate can reuse
// mountBundleFixture rather than inventing a fifth way to do this.

// bundleDomain is unique to this file. A domain colliding with a core embedded
// domain is silently skipped by MountRuntimeDomainsFromEnv (the embedded tree
// owns the namespace), which would make every assertion below pass vacuously.
const bundleDomain = "relgate3659"

// mountBundleFixture writes concepts.memql into <tmp>/<bundleDomain>/, mounts it
// through MEMQL_DSL_PATH, loads concepts, and runs engine Init -- the same
// sequence app/database.go and app/engine.go perform at boot. It returns Init's
// error so a caller can assert on the refusal.
//
// Both process-global mutables are restored: dsl.pluginTrees via UnregisterTree,
// and the concept registry via ReplaceAll. The registry needs it independently
// because LoadUnifiedConcepts MERGES additively and Init normalizes definitions
// in place onto shared pointers, so a fixture left behind corrupts every later
// test in the process.
func mountBundleFixture(t *testing.T, body string) error {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, bundleDomain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "concepts.memql"), []byte(body), 0o644); err != nil {
		t.Fatalf("write bundle fixture: %v", err)
	}

	before := concept.All()
	t.Cleanup(func() { concept.ReplaceAll(before) })

	t.Setenv("MEMQL_DSL_PATH", root)
	mounted := memqldsl.MountRuntimeDomainsFromEnv(nil)
	t.Cleanup(func() {
		for _, d := range mounted {
			memqldsl.UnregisterTree(d)
		}
	})

	// Guards against a vacuous pass: if the domain were skipped as a core
	// collision or a "_" prefix, nothing below would be under test.
	var found bool
	for _, d := range mounted {
		if d == bundleDomain {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("bundle domain %q was not mounted (mounted=%v) -- the fixture is not under test", bundleDomain, mounted)
	}

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	return newQuietEngine(t).Init(concept.DefaultRegistry())
}

// TestMountedBundleWithValidRelationshipLoads is the positive control, and it
// carries more weight than usual here: without it, a harness that failed for
// some unrelated reason (a malformed fixture, an unmountable domain) would make
// every negative case below pass while testing nothing.
func TestMountedBundleWithValidRelationshipLoads(t *testing.T) {
	err := mountBundleFixture(t, `
@description("A hub other rows point at.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget pointing at its hub.")
concept gadget {
  hubId  string  @description("FK to the owning hub.")

  @relationship(type="interactsWith", as="belongsToHub", field="hubId", target=hub, direction="outgoing")
}
`)
	if err != nil {
		t.Fatalf("a valid product bundle was refused: %v", err)
	}
}

// TestMountedBundleRelationshipGates walks every relationship invariant this
// epic enforces and asserts each one REFUSES a mounted bundle.
//
// Each case is a defect a product repo could plausibly ship, and each was
// previously invisible to them: this repo's gates are Go tests over dsl/, which
// no downstream bundle is covered by (memql#3629).
func TestMountedBundleRelationshipGates(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantsMsg string
	}{
		{
			name: "unknownStructuralType",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget with a domain verb in the structural slot.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="assignedTo", field="hubId", target=hub, direction="outgoing")
}
`,
			// The message must name the way OUT, not merely refuse (memql#3652).
			wantsMsg: `as="assignedTo"`,
		},
		{
			name: "malformedAsLabel",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget with a malformed domain label.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="interactsWith", as="Belongs_To_Hub", field="hubId", target=hub, direction="outgoing")
}
`,
			wantsMsg: "lowerCamelCase",
		},
		{
			name: "unresolvableTarget",
			body: `
@description("A gadget pointing at a concept that does not exist.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="interactsWith", field="hubId", target="v1:relgate3659:hubb", direction="outgoing")
}
`,
			wantsMsg: "not a registered concept",
		},
		{
			name: "fieldNotDeclared",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget whose relationship names a field it does not declare.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="interactsWith", field="hubbId", target=hub, direction="outgoing")
}
`,
			wantsMsg: "is not declared",
		},
		{
			name: "caseMismatchedField",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget whose relationship field differs only in case.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="interactsWith", field="HubId", target=hub, direction="outgoing")
}
`,
			wantsMsg: "is not declared",
		},
		{
			name: "invalidDirection",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget with a bogus direction.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="interactsWith", field="hubId", target=hub, direction="sideways")
}
`,
			wantsMsg: "direction",
		},
		{
			name: "duplicateRelationshipOnOneField",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget declaring the same type twice on one field.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="interactsWith", field="hubId", target=hub, direction="outgoing")
  @relationship(type="interactsWith", as="second", field="hubId", target=hub, direction="outgoing")
}
`,
			wantsMsg: "duplicate",
		},
		{
			name: "containsDeclaredIncoming",
			body: `
@description("A hub.")
concept hub {
  name  string  @description("Hub name.")
}

@description("A gadget declaring contains with an incoming direction.")
concept gadget {
  hubId  string  @description("FK.")

  @relationship(type="contains", field="hubId", target=hub, direction="incoming")
}
`,
			wantsMsg: "contains",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mountBundleFixture(t, tc.body)
			if err == nil {
				t.Fatalf("a mounted bundle with %s loaded clean -- product bundles are ungated for this invariant", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantsMsg) {
				t.Errorf("error %q does not contain %q -- a downstream author cannot read our Go, so the message is the whole remedy", err.Error(), tc.wantsMsg)
			}
		})
	}
}
