package main

import (
	"testing"

	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/secret"
)

// TestApplySubcommandEnv_AutoloadDisabledIsNoop: with MEMQL_GENESIS_AUTOLOAD
// unset the helper is a no-op and returns nil, so local-dev / Container-App
// override paths are untouched.
func TestApplySubcommandEnv_AutoloadDisabledIsNoop(t *testing.T) {
	t.Setenv(genesis.EnvAutoload, "")
	if err := applySubcommandEnv("test"); err != nil {
		t.Fatalf("applySubcommandEnv with autoload disabled: %v", err)
	}
}

// TestApplySubcommandEnv_FailsClosedOnBadEnvelope is the #751 regression guard:
// the subcommand bootstrap must INVOKE the genesis envelope autoload (it didn't
// before -- only ApplyLocalOverride ran, so envelope-sealed values like
// IDENTITY_SIGNING_KEY_B64 were never decrypted and the service_account mint
// failed). When autoload is requested but misconfigured (no master key), the
// helper must FAIL CLOSED -- proving the autoload is wired in, and that a
// credential mint aborts rather than proceeding on a half-applied config.
func TestApplySubcommandEnv_FailsClosedOnBadEnvelope(t *testing.T) {
	t.Setenv(genesis.EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, "") // flag=true but no key -> AutoloadFromEnv errors
	err := applySubcommandEnv("test")
	if err == nil {
		t.Fatal("expected applySubcommandEnv to fail closed when the envelope autoload errors")
	}
}
