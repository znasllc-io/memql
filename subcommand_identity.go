//go:build identity

package main

import (
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// dispatchSubcommand routes operator subcommands available on the identity
// binary. Returns (true, exitCode) when the first arg names a known
// subcommand; otherwise (false, 0) so main() falls through to the normal
// server bootstrap.
//
// It lived beside the voice-agent-token subcommand, which was the first one
// the identity binary carried. That subcommand went with the voice node
// (epic memql#4988); this table and selectMintDependencies below serve the
// node-token and service-account-token mints, which are unrelated to it.
func dispatchSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "migrate":
		return true, runMigrateSubcommand(args[1:])
	// Available on EVERY node binary (epic memql#4794): it is the init
	// container that runs before a node boots, and every DSL-consuming node
	// type needs it. It reads object storage and the filesystem, never the
	// database, so it has no build-tag dependencies of its own.
	case "dsl-fetch":
		return true, runDslFetchSubcommand(args[1:])
	case "pat":
		return true, runPATSubcommand(args[1:])
	case "enrolment-token":
		return true, runEnrolmentTokenSubcommand(args[1:])
	case "recovery-key":
		return true, runRecoveryKeySubcommand(args[1:])
	case "node-token":
		return true, runNodeTokenSubcommand(args[1:])
	case "service-account-token":
		return true, runServiceAccountTokenSubcommand(args[1:])
	// Available on EVERY node binary (memql#4335): it reports the VENDOR
	// credential this particular node holds, and the answer can differ per
	// node, which is what makes running it per node worth anything.
	case "provider-auth":
		return true, runProviderAuthSubcommand(args[1:])
	}
	return false, 0
}

// selectMintDependencies returns the subset of bootstrap deps a mint
// subcommand actually needs (database + engine + identity service + the
// supporting components). Filters out the HTTP + gRPC transport servers so
// the CLI does not try to bind the same ports as the running identity
// container -- the subcommand is typically `kubectl exec`'d into it.
func selectMintDependencies(all []common.Dependency) []common.Dependency {
	keep := []common.Dependency{}
	for _, d := range all {
		name := strings.ToLower(string(d.ComponentName()))
		// Skip anything whose name implies a network listener -- the live
		// container already owns those ports. The bootstrap phases name
		// servers consistently enough that a substring match is reliable
		// (grpcServer, httpServer, etc.).
		if strings.Contains(name, "server") {
			continue
		}
		keep = append(keep, d)
	}
	return keep
}
