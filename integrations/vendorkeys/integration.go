package vendorkeys

import (
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
}

// NewIntegration builds the integration over a store and its resolver.
func NewIntegration(store *Store, resolver *Resolver, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{store: store, resolver: resolver, logger: logger}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "vendorkeys" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability { return nil }
