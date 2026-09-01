package packages

import (
	"testing/fstest"
)

// The fixture packages (design section J).
//
// Built in Go over fstest.MapFS rather than checked in as testdata trees, for
// one reason that matters: every fixture below differs from the valid one by a
// SINGLE stated fact, and a builder makes that difference readable at the call
// site instead of a diff between two directories. A reader of
// brokenManifestPackage can see it is validPackage with one line changed.
//
// Nothing here reaches a cluster, a database, or the network.

const validManifest = `formatVersion: 1
name: acme
deployables:
  - name: storefront
    path: clients/web
    kind: shopify_storefront
    binding:
      storeDomain: acme.myshopify.com
      storefrontTokenRef: acme-storefront-token
  - name: docs
    path: clients/docs
    kind: static
`

// A concept the engine's own Init accepts: an ordinary product concept with a
// declared row-authz tier and no relationships.
const validConcepts = `/// An acme widget.
@rowAuthz(owner="ownerUserId", clusterOwner)
concept widget {
  ownerUserId  string!  @description("Owner of this widget.")
  label        string   @description("What to call it.")
}
`

// The same concept with a NON-CANONICAL @relationship type. `assignedTo` is a
// domain verb, and the closed engine set is parent/owns/createdBy/alias/
// equals/contains/references -- so Init refuses the tree rather than skipping
// the construct. This is the witness lint_parity.go names, which is why it is
// the one used here: a fixture that fails for a reason nobody has measured is
// a negative control that proves nothing.
const bootRefusingConcepts = `/// An acme widget with an uncanonical relationship type.
@rowAuthz(owner="ownerUserId", clusterOwner)
concept widget {
  ownerUserId  string!  @description("Owner of this widget.")
  ownerRef     string   @description("Points at the owner.")

  @relationship(type="assignedTo", field="ownerRef", target=widget, direction="outgoing")
}
`

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// validPackage is a memql-project-shaped tree: a manifest, two declared
// deployables, one discovered DSL domain.
func validPackage() fstest.MapFS {
	return fstest.MapFS{
		ManifestName:                file(validManifest),
		"clients/web/package.json":  file(`{"name":"web"}`),
		"clients/web/src/main.tsx":  file("// entry\n"),
		"clients/docs/package.json": file(`{"name":"docs"}`),
		"clients/docs/index.html":   file("<!doctype html>\n"),
		"dsl/acme/concepts.memql":   file(validConcepts),
	}
}

// prebuiltPackage is validPackage with a built tree already in the snapshot --
// the D4 fast-path's analysis half.
func prebuiltPackage() fstest.MapFS {
	p := validPackage()
	p["clients/web/dist/index.html"] = file("<!doctype html><title>acme</title>\n")
	p["clients/web/dist/assets/app.js"] = file("console.log(1)\n")
	return p
}

// goPackPackage is validPackage plus a bff/ with a go.mod (D3).
func goPackPackage() fstest.MapFS {
	p := validPackage()
	p["bff/go.mod"] = file("module github.com/acme/bff\n\ngo 1.26\n")
	p["bff/plugin.go"] = file("package bff\n")
	return p
}

// bootRefusingPackage is validPackage whose DSL would refuse boot (D12).
func bootRefusingPackage() fstest.MapFS {
	p := validPackage()
	p["dsl/acme/concepts.memql"] = file(bootRefusingConcepts)
	return p
}

// reservedDomainPackage ships a DSL domain the engine already owns.
func reservedDomainPackage() fstest.MapFS {
	p := validPackage()
	p["dsl/cognition/concepts.memql"] = file(validConcepts)
	return p
}
