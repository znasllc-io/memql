//go:build agent

package app

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// clusterAccountPaysForThePeopleHere says, for a fixture whose subject is not
// who pays, that the cluster's own vendor account pays for the people in it.
//
// WHY A FIXTURE HAS TO SAY SO. The work fixtures register one vendor record
// and make a person's calls through it. The cluster's account pays for a
// person only when its owner allowed them (owner decision 2026-11-22,
// component/memql vendor_account_cluster.go), and a registry a test builds has
// no gate, which is closed to every person. Without this line every such call
// is refused before the behaviour under test is reached.
//
// The rule itself is tested where it lives: component/router
// (accounts_test.go), component/memql (cluster_account_gate_db_test.go) and,
// across two replicas, integrations/vendorkeys (hop_db_test.go).
func clusterAccountPaysForThePeopleHere(registry *memql.ProviderRegistry) {
	registry.SetClusterAccountGate(func(context.Context, string) bool { return true })
}
