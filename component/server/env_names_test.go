package server

import "testing"

// The env names component/server actually composes, pinned as literals.
//
// WHY THIS TEST EXISTS AND WHAT IT WOULD HAVE PREVENTED (memql#3892).
//
// These variables are read as `env.NewEnvReader(prefix).String(keys.Field)` --
// a prefix held in a constant and a key held in a struct field. The composed
// name therefore appears in NO source file, and three separate things that look
// like they would notice cannot:
//
//   - `grep -rn 'SERVER_ADDRESS' --include='*.go'` returns nothing. memql#3892
//     was filed on exactly that evidence and concluded the variable was set on
//     every pod and read by nothing. It was setting the HTTP listen address on
//     every node. Acting on "dead variable" would have deleted three working
//     operator knobs.
//   - envscan cannot attribute the read either; these lines sit in its
//     unresolvable residual (memql#3834), so the drift gate stayed green about
//     a whole unregistered family.
//   - TestOwnedVarsArePrefixed only inspects REGISTRY entries, so a family that
//     was never registered was never in scope for it.
//
// A test that asserts the composed name is the one thing that closes the loop:
// it fails if the prefix or a key ever changes without the registry, the alias
// map and the deploy manifests changing with it. It deliberately asserts the
// LITERAL strings rather than deriving them from the same constants the code
// uses -- a derived expectation would rename itself alongside the bug.
func TestServerEnvNamesAreTheRegisteredOnes(t *testing.T) {
	t.Setenv("MEMQL_SERVER_ADDRESS", "0.0.0.0:9999")
	t.Setenv("MEMQL_SERVER_ALLOWED_ORIGINS", "https://example.test, https://other.test")
	t.Setenv("MEMQL_SERVER_READ_TIMEOUT_MS", "1234")
	t.Setenv("MEMQL_SERVER_WRITE_TIMEOUT_MS", "4321")

	opts, err := loadDefaultServerEnvOptions()
	if err != nil {
		t.Fatalf("load server env options: %v", err)
	}

	if opts.Address != "0.0.0.0:9999" {
		t.Errorf("MEMQL_SERVER_ADDRESS not read: Address = %q, want %q", opts.Address, "0.0.0.0:9999")
	}
	if got, want := len(opts.AllowedOrigins), 2; got != want {
		t.Errorf("MEMQL_SERVER_ALLOWED_ORIGINS not read: got %d origins %v, want %d", got, opts.AllowedOrigins, want)
	}
	if opts.ReadTimeoutMs == nil || *opts.ReadTimeoutMs != 1234 {
		t.Errorf("MEMQL_SERVER_READ_TIMEOUT_MS not read: %v", opts.ReadTimeoutMs)
	}
	if opts.WriteTimeoutMs == nil || *opts.WriteTimeoutMs != 4321 {
		t.Errorf("MEMQL_SERVER_WRITE_TIMEOUT_MS not read: %v", opts.WriteTimeoutMs)
	}

	// And the value has to survive the trip into the server config, because
	// "the option was parsed" and "the listener moved" are different claims and
	// only the second one is what an operator setting this wants.
	args, err := EnvOptionsToArgs(opts)
	if err != nil {
		t.Fatalf("env options to args: %v", err)
	}
	cfg := defaultConfig()
	for _, arg := range args {
		arg.Apply(cfg)
	}
	if cfg.address != "0.0.0.0:9999" {
		t.Errorf("effective listen address = %q, want %q", cfg.address, "0.0.0.0:9999")
	}
	if len(cfg.allowedOrigins) != 2 {
		t.Errorf("effective allowed origins = %v, want 2 entries", cfg.allowedOrigins)
	}
}

// The PRE-CONVENTION spellings must no longer be read directly.
//
// They keep working for operators, but through genesis.ApplyLegacyEnvAliases at
// boot -- which copies the legacy value onto the new name before any config is
// read -- not because this package still looks for them. Asserting the negative
// here is what makes the alias shim load-bearing rather than decorative: if the
// prefix ever reverted, this test would pass a value through and fail.
//
// component/genesis owns the other half (TestServerServiceLegacyAliasesBridge),
// because the bridge is its code and importing it here would be a cycle.
func TestServerEnvIgnoresPreConventionNames(t *testing.T) {
	t.Setenv("SERVER_ADDRESS", "0.0.0.0:7777")
	t.Setenv("SERVER_ALLOWED_ORIGINS", "https://legacy.test")

	opts, err := loadDefaultServerEnvOptions()
	if err != nil {
		t.Fatalf("load server env options: %v", err)
	}

	if opts.Address != "" {
		t.Errorf("bare SERVER_ADDRESS was read directly (= %q); it must arrive via the boot alias shim, not this reader", opts.Address)
	}
	// Not "empty": LoadServerEnvOptions substitutes the "*" default for an
	// unset list before returning, so absence reads as ["*"] here. What must
	// not appear is the legacy variable's VALUE.
	for _, origin := range opts.AllowedOrigins {
		if origin == "https://legacy.test" {
			t.Errorf("bare SERVER_ALLOWED_ORIGINS was read directly (= %v); it must arrive via the boot alias shim", opts.AllowedOrigins)
		}
	}
}
