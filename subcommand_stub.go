//go:build !identity

package main

// dispatchSubcommand returns (true, exitCode) only for subcommands
// implemented on this binary. The identity binary carries the credential
// mints (see subcommand_identity.go); the other node binaries route through
// their normal server bootstrap.
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
	case "backup":
		return true, runBackupSubcommand(args[1:])
	// Available on EVERY node binary (memql#4335): it reports the VENDOR
	// credential this particular node holds, and the answer can differ per
	// node, which is what makes running it per node worth anything.
	case "provider-auth":
		return true, runProviderAuthSubcommand(args[1:])
	}
	return false, 0
}
