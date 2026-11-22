package vendorkeys

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/memql"
)

// integration.go -- a person's API keys at a model vendor (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md).
//
// An API key is the second kind of account that can pay for a vendor call; the
// first is the cluster's own workload identity federation. A key belongs to
// the person who added it, who may lend it. The cluster's own work never
// spends one, and there is no cluster key.
//
// This package is the only reader and writer of the two rows a key is
// (v1:router:vendorKey, v1:router:vendorKeyState). It holds three things:
//
//   - the STORE (store.go): the graph seam, every read fresh and as the
//     cluster acting, the sealed value read by one function;
//   - the RESOLVER (resolver.go, observe.go): memql.VendorKeys, the seam the
//     provider registry asks which keys can pay for a call, and tells what a
//     call learned about one. Installed on every node, because a call a key
//     pays for is made by whichever replica runs it;
//   - the CAPABILITIES a person reaches a key through. Every one of them
//     decides who is asking before it reads or writes anything, because the
//     rows are readable by no client and the queries beneath are @serverOnly.
//
// It is a COMPONENT in the module taxonomy and not an integration, by the test
// module_taxonomy_test.go states: it calls nobody's API through this file. It
// reads and writes this cluster's own graph. (The check that asks a vendor
// whether it accepts a key is component/memql's ProbeVendorKey.)

// Integration exposes the key capabilities.
type Integration struct {
	store    *Store
	resolver *Resolver
	logger   *slog.Logger
	// probe asks a vendor whether it accepts a key, by listing its models
	// (memql.ProviderRegistry.ProbeVendorKey). A field so a test can stand a
	// vendor in without a server.
	probe func(ctx context.Context, vendor, key string) memql.VendorKeyProbe
}

// NewIntegration builds the integration over a store and its resolver.
// providers is the node's provider registry, which knows where this cluster
// sends each vendor's calls and makes the check; with none, every check
// answers that the vendor could not be reached, and no key is saved.
func NewIntegration(store *Store, resolver *Resolver, providers *memql.ProviderRegistry, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	i := &Integration{store: store, resolver: resolver, logger: logger}
	i.probe = func(ctx context.Context, vendor, key string) memql.VendorKeyProbe {
		if providers == nil {
			return memql.VendorKeyProbe{
				Verdict: memql.VendorKeyProbeUnreachable,
				Detail:  "This node cannot reach a model vendor, so the key could not be checked.",
			}
		}
		return providers.ProbeVendorKey(ctx, vendor, key)
	}
	return i
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "vendorkeys" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "add",
			Description: "Add an API key for Anthropic or OpenAI as the caller's own. The key is checked by listing the vendor's models, " +
				"sealed and stored. Returns the key's card, which never carries the key.",
			Handler: i.handleAdd,
			ArgsSchema: map[string]string{
				"vendor": "string (required) -- anthropic or openai.",
				"label":  "string (required) -- the owner's name for the key, 1 to 60 characters.",
				"key":    "string (required) -- the API key. Read once, checked, sealed and discarded.",
			},
		},
		{
			Name:        "forActor",
			Description: "The keys the caller can see: their own, short cards of keys lent to them, and for a cluster owner who asks, every active key.",
			Handler:     i.handleForActor,
			ArgsSchema: map[string]string{
				"all": "boolean (optional) -- a cluster owner may ask for every active key on the cluster.",
			},
		},
		{
			Name:        "update",
			Description: "Change the owner's settings on one of their keys: its name, whether it is the default at its vendor, and whether Auto may use it.",
			Handler:     i.handleUpdate,
			ArgsSchema: map[string]string{
				"keyId":     "string (required) -- the key to change.",
				"label":     "string (optional) -- a new name, 1 to 60 characters.",
				"isDefault": "boolean (optional) -- true makes it the owner's default at its vendor.",
				"autoUse":   "boolean (optional) -- whether Auto may pay with it.",
			},
		},
		{
			Name:        "check",
			Description: "Check a key now by listing its vendor's models with it, which sends no prompt. The key's owner or a cluster owner may.",
			Handler:     i.handleCheck,
			ArgsSchema:  map[string]string{"keyId": "string (required) -- the key to check."},
		},
		{
			Name:        "remove",
			Description: "Remove a key. It stops paying at once and is never used again; the record stays. The key's owner or a cluster owner may.",
			Handler:     i.handleRemove,
			ArgsSchema:  map[string]string{"keyId": "string (required) -- the key to remove."},
		},
	}
}
