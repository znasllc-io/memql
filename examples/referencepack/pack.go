// Package referencepack is the minimal REFERENCE PACK for the memQL pack
// model (epic 2, issue 2.5). It demonstrates, end-to-end, every primitive a
// real product pack uses to drop into the memQL engine:
//
//   - dsl.RegisterTree(domain, Tree())   -- mount an embedded .memql subtree
//     (concept + builtin + tool + automation) under its own namespace.
//   - memql.RegisterPluginForContract(...) -- register a Go IntegrationProvider
//     against an explicit Plugin SDK contract version, so the loader rejects a
//     stale pack at startup instead of silently mis-binding.
//   - an IntegrationProvider whose Capabilities() back the pack's builtin via
//     the @executor("integration.referencepack.composeGreeting") FQN.
//
// It is the worked example for docs/public/build/building-a-pack.md and the
// contract reference docs/public/build/plugin-sdk.md.
//
// NOT IN PRODUCTION -- BY DESIGN.
//
// This package is NORMAL (untagged) Go, so `go build ./...` compiles it and CI
// verifies it builds. But NO production binary imports it, so it never ships.
// Crucially, there is no unconditional self-registering init() in this file:
// linking the package in does NOT load the pack. Registration is opt-in:
//
//   - Tests / examples call Register(Domain) (or the lower-level primitives)
//     explicitly.
//   - register_referencepack.go carries a //go:build referencepack init() that
//     calls Register(Domain) -- the REAL build-tag-gated auto-register pattern a
//     production pack uses. The `referencepack` tag is never set in prod, so the
//     pack never auto-loads there.
//
// A production pack would instead put its init() behind its product build tag
// (e.g. //go:build copresent) and anchor the package via a blank import in the
// app bootstrap. See docs/public/build/building-a-pack.md.
package referencepack

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns. It must not collide with a core
// embedded domain or with another pack's domain -- dsl.RegisterTree validates
// this via dsl.ValidatePackDomain and panics on a collision (namespace
// ownership, issue 2.4). The embedded subtree is mounted under "referencepack/"
// in the unified DSL tree.
const Domain = "referencepack"

// ContractVersion is the Plugin SDK contract version this pack was built
// against. A production pack pins this explicitly (via RegisterPluginForContract)
// so the loader fails loudly if the pack is linked into a core whose
// memql.PluginContractVersion is incompatible (see CheckPluginContractCompat /
// PluginRegistration.ValidateContract). We pin the value the pack was authored
// against by referencing the constant the pack compiled with.
const ContractVersion = memql.PluginContractVersion

// integrationName is the IntegrationProvider's stable identifier. It is the
// middle segment of every capability FQN: "integration.<integrationName>.<cap>".
// It MUST match the @executor namespace used in dsl/builtins.memql.
const integrationName = "referencepack"

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree, rooted so that the files in
// examples/referencepack/dsl/ appear directly (concepts.memql, builtins.memql,
// ...). Pass this to dsl.RegisterTree(Domain, Tree()) to mount it. Returning an
// fs.FS (not the raw embed.FS) keeps the rooting an implementation detail.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		// The embed is baked into the binary; a failure here is a build defect,
		// not a runtime condition.
		panic("referencepack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Provider is the pack's IntegrationProvider. It exposes one DSL-callable
// capability ("composeGreeting") that the pack's builtin
// (referencePackComposeGreeting) is wired to via @executor.
type Provider struct{}

// IntegrationName implements memql.IntegrationProvider.
func (p *Provider) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider. The one capability's Name
// ("composeGreeting") combines with IntegrationName() into the FQN
// "integration.referencepack.composeGreeting" -- exactly the @executor the
// builtin in dsl/builtins.memql names.
func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "composeGreeting",
			Description: "Compose a greeting string for a user name. The reference pack's single Go-backed capability.",
			Handler:     p.composeGreeting,
			ArgsSchema: map[string]string{
				"userName": "string (required) - name to greet",
			},
		},
	}
}

// composeGreeting is the capability handler. It reuses the builtin executor
// handler signature so it slots directly into the engine's dispatch path. It
// returns a single object node carrying the composed greeting.
func (p *Provider) composeGreeting(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	userName, _ := args["userName"].(string)
	if userName == "" {
		return nil, fmt.Errorf("referencepack.composeGreeting requires userName")
	}
	payload, _ := json.Marshal(map[string]any{
		"greeting":    fmt.Sprintf("Hello, %s -- from the reference pack.", userName),
		"composedAt":  time.Now().UTC().Format(time.RFC3339),
		"integration": integrationName,
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("referencepack-greeting:%s", userName),
		Concept:   "integration:referencepack:greeting",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// NewProvider builds the pack's IntegrationProvider from a PluginContext. It is
// the memql.PluginFactory a production pack passes to RegisterPluginForContract.
// This pack needs nothing from pctx beyond what every pack receives, so the
// factory is trivial; a real pack would pluck DB getters, providers, and
// resolvers off pctx here. Returning (nil, nil) is the documented opt-out for a
// pack whose dependencies are not satisfied in the current environment.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	_ = pctx // intentionally unused: the reference capability has no external deps
	return &Provider{}, nil
}

// Register wires the pack into the engine registries. It is the single
// entry point a build-tag-gated init() (or a test) calls. It performs the two
// registration primitives a pack needs:
//
//  1. dsl.RegisterTree(domain, Tree()) -- mounts the embedded .memql subtree
//     under domain/. Namespace ownership is validated here; a collision panics.
//  2. memql.RegisterPluginForContract(domain, ContractVersion, NewProvider) --
//     registers the Go IntegrationProvider against the pinned contract version,
//     which the app bootstrap validates against memql.PluginContractVersion
//     before materializing the pack.
//
// (A pack that crosses node boundaries with events would also call
// node.RegisterRoutingRule here; this minimal pack does not.)
//
// domain is a parameter (rather than hardcoded Domain) so a test can register
// the same tree under a throwaway, unique namespace and tear it down with
// dsl.UnregisterTree without fighting another test for the canonical name.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}
