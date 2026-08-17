package main

import (
	"os"
	"testing"
)

// subcommand_env_test.go -- the #751 guard, rewritten for one delivery path
// (epic memql#3958).
//
// WHAT THESE TESTS USED TO ASSERT, AND WHY IT IS GONE. Both were about the
// sealed genesis envelope: one that `applySubcommandEnv` was a no-op with
// MEMQL_GENESIS_AUTOLOAD unset, the other that it FAILED CLOSED when autoload
// was requested with no master key. The envelope no longer exists, so those
// assertions have no subject.
//
// THE CONCERN THEY EXISTED FOR SURVIVES, AND IS WHAT IS TESTED HERE. #751 was
// never really about envelopes: main() dispatches subcommands BEFORE it runs
// the boot-time env layering, so a subcommand that skipped that layering saw a
// different environment than the node running beside it -- and failed to mint
// credentials it should have been able to mint. The envelope was one of those
// layers; three remain, and dropping any of them reproduces #751 exactly.
//
// So these assert the layers are WIRED, by observing each one's effect. A test
// that merely called the function and checked for a nil error would pass with
// the whole body deleted, which is the shape of the original defect.

// TestApplySubcommandEnvBridgesLegacyNames proves ApplyLegacyEnvAliases runs.
//
// The live case #751 recorded: the voice-agent subcommand fail-fast'd on the
// required MEMQL_OPENAI_API_KEY against a cluster still carrying the legacy
// spelling, because nothing bridged the two on the subcommand path.
func TestApplySubcommandEnvBridgesLegacyNames(t *testing.T) {
	const legacy = "MEMQL_SI_OPENAI_API_KEY"
	const modern = "MEMQL_AI_OPENAI_API_KEY"

	t.Setenv(legacy, "value-from-the-legacy-name")
	t.Setenv(modern, "")
	if err := os.Unsetenv(modern); err != nil {
		t.Fatalf("unset %s: %v", modern, err)
	}

	if err := applySubcommandEnv("test"); err != nil {
		t.Fatalf("applySubcommandEnv: %v", err)
	}

	if got := os.Getenv(modern); got != "value-from-the-legacy-name" {
		t.Errorf("%s = %q after applySubcommandEnv, want the legacy value bridged onto it.\n"+
			"The legacy-alias layer is not wired into the subcommand path, so a subcommand sees "+
			"a different environment than the node beside it -- which is memql#751 exactly, "+
			"whichever layer went missing.", modern, got)
	}
}

// TestApplySubcommandEnvDerivesTheDomain proves ApplyDomainDerivations runs.
//
// A subcommand that minted a credential against a different issuer than the
// node verifies with would produce a token nothing accepts, with every
// manifest looking correct (memql#3593).
func TestApplySubcommandEnvDerivesTheDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "lab.example.com")
	for _, derived := range []string{
		"MEMQL_IDENTITY_BASE_URL",
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER",
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN",
	} {
		t.Setenv(derived, "")
		if err := os.Unsetenv(derived); err != nil {
			t.Fatalf("unset %s: %v", derived, err)
		}
	}

	if err := applySubcommandEnv("test"); err != nil {
		t.Fatalf("applySubcommandEnv: %v", err)
	}

	if got := os.Getenv("MEMQL_IDENTITY_BASE_URL"); got != "https://identity.lab.example.com" {
		t.Errorf("MEMQL_IDENTITY_BASE_URL = %q, want it derived from MEMQL_DOMAIN.\n"+
			"The domain-derivation layer is not wired into the subcommand path, so a subcommand "+
			"mints against a different issuer than the node verifies with.", got)
	}
	if got := os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN"); got != "lab.example.com" {
		t.Errorf("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN = %q, want %q", got, "lab.example.com")
	}
}

// TestApplySubcommandEnvSucceedsWithNothingSet is the ordinary path: no /.env,
// no MEMQL_DOMAIN, nothing to bridge. It must return nil rather than treating
// an absent optional layer as a failure.
func TestApplySubcommandEnvSucceedsWithNothingSet(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	if err := os.Unsetenv("MEMQL_DOMAIN"); err != nil {
		t.Fatalf("unset MEMQL_DOMAIN: %v", err)
	}
	if err := applySubcommandEnv("test"); err != nil {
		t.Fatalf("applySubcommandEnv with nothing set: %v", err)
	}
}
