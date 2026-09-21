// Package wholesalepack is the client-agnostic wholesale pack (epic
// memql#5533).
//
// It is a PACK, not core: dsl/commerce stays an engine domain and this
// package cannot shadow it. It links into every binary with no build tag,
// exactly as packs/reviewspack does, and its reach is governed by
// v1:platform:packState (epic memql#5532, issue memql#5549).
package wholesalepack

import (
	"io/fs"

	"embed"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns.
const Domain = "wholesale"

// ContractVersion is the Plugin SDK contract this pack was built against.
const ContractVersion = memql.PluginContractVersion

const integrationName = "wholesale"

// DefaultEnabled is what this pack ships as when no v1:platform:packState
// row governs it (epic memql#5532, issue memql#5549).
//
// FALSE, for the same reason reviewspack ships disabled and one more of
// its own: enabling this pack publishes a write endpoint on every
// deployable whose shopperForms is on, and what that endpoint collects is
// a named person's business contact details. That is an operator's
// decision, not an upgrade's.
const DefaultEnabled = false

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("wholesalepack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Provider is the pack's IntegrationProvider.
//
// THREE SEAMS AND NOT AN ENGINE HANDLE PASSED AROUND. Every capability in
// this pack is a decision over rows plus, for two of them, a call to a
// construct -- so the Go half holds a reader for the first and a caller for
// the second, and the tests exercise both as functions over values rather
// than over an engine envelope. `engine` is kept because the seams are
// built from it and a future capability may need it directly.
type Provider struct {
	engine memql.IntegrationEngineAccess
	reader rowReader
	caller Caller
}

func (p *Provider) IntegrationName() string { return integrationName }

func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "submitApplication",
			Description: "The shopper write path: a business applies for trade terms. Refused " +
				"when this store's applicationsOpen is false, and absent settings are closed.",
			Handler: p.submitApplication,
			ArgsSchema: map[string]string{
				"storeId":        "string (required) - stamped from the binding the post arrived through",
				"siteId":         "string (optional) - provenance",
				"companyName":    "string (required) - the business applying",
				"applicantName":  "string (required) - the person applying",
				"applicantEmail": "string (required) - unverified; how a decision reaches them",
			},
		},
		{
			Name: "recordDecision",
			Description: "Append one decision to an application's log. Client principal only; " +
				"refuses an illegal transition by name; copies storeId off the application.",
			Handler: p.recordDecision,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application being decided",
				"transition":    "string (required) - approve, reject or revoke",
				"principalKind": "string (required) - must be client",
				"decidedBy":     "string (required) - client user id",
				"note":          "string (optional) - the reason, which survives the next decision",
			},
		},
		{
			Name: "applicationState",
			Description: "The derived state of one application, folded from its append-only " +
				"decision log, and what is legal next.",
			Handler: p.applicationState,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application to fold",
			},
		},
		{
			Name:        "setWholesaleSettings",
			Description: "Write one store's wholesale settings. Data, not source -- no rebuild.",
			Handler:     p.setWholesaleSettings,
			ArgsSchema: map[string]string{
				"storeId":            "string (required) - the store these settings govern",
				"applicationsOpen":   "bool (required) - whether the shopper form accepts posts",
				"entitlementAdapter": "string (optional) - the named adapter; refused when unknown",
				"introText":          "string (optional) - client-authored storefront copy",
			},
		},
		{
			Name: "provisionEntitlement",
			Description: "Grant wholesale prices through this store's chosen adapter. Only an " +
				"APPROVED application has one to provision. A failed push leaves the " +
				"application approved and the entitlement pending with its error.",
			Handler: p.provisionEntitlement,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the approved application",
			},
		},
		{
			Name: "revokeEntitlement",
			Description: "Take one back, through the adapter that GRANTED it rather than " +
				"whatever settings name today. Requires a revoke decision to have been " +
				"recorded first, so the history and the state agree.",
			Handler: p.revokeEntitlement,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the revoked application",
			},
		},
	}
}

// NewProvider builds the pack IntegrationProvider.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	return &Provider{
		engine: pctx.Engine,
		reader: &engineRowReader{engine: pctx.Engine},
		caller: &engineCaller{engine: pctx.Engine},
	}, nil
}

// Register wires the pack into the engine registries.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
	registerShopperSurface()
	// Bind the Go half to the pack domain so a v1:platform:packState
	// disable skips the factory and the module inventory folds this
	// integration under its pack row (memql#4183). Contract packs register
	// the plugin under the domain name, so the pair is (domain, domain).
	memql.BindPluginToPack(domain, domain)
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
	registerShippedAdapters()
}

// registerShippedAdapters adds the two adapters this pack ships.
//
// IN Register, NOT IN init(), for the reason the whole pack loads this way:
// a cluster that has this pack disabled runs no behavioural half at all,
// and an adapter registered from a package init would be listed in the
// module inventory of a cluster that cannot use it. It is also what lets a
// test register a fake adapter against a clean registry.
func registerShippedAdapters() {
	if _, err := AdapterByName(AdapterShopifyB2B); err != nil {
		RegisterAdapter(NewShopifyB2BAdapter())
	}
	if _, err := AdapterByName(AdapterCustomerTag); err != nil {
		RegisterAdapter(NewCustomerTagAdapter(""))
	}
}
