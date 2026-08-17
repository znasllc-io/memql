package envregistry

import (
	"os"
	"testing"
)

// The SERVER_* / SERVICE_* bridge (memql#3892) and the voice-agent's five (memql#3834).
//
// Moved here from component/genesis with its subject (memql#3963): the
// registry half of that package -- the manifest, boot validation, domain
// derivations and this legacy-alias shim -- became component/envregistry, and
// the sealed-envelope half it shared a directory with was deleted outright
// (epic memql#3958). A test in the package that no longer declares
// ApplyLegacyEnvAliases is a build failure, not a smaller diff.
//
// component/server and component/service moved their env prefixes onto the
// MEMQL_ convention so the family could be REGISTERED at all -- the registry
// refuses a non-MEMQL_ owned entry, which is why a live family stayed invisible
// to the drift gate for its whole life. That rename is only safe because this
// shim carries an operator's existing spelling forward, so the two halves are
// asserted together rather than trusted.
//
// The pairing matters: component/server's TestServerEnvIgnoresPreConventionNames
// asserts the reader no longer looks at the bare name, and this asserts the bare
// name still arrives. Either test alone describes a broken system.
func TestServerServiceLegacyAliasesBridge(t *testing.T) {
	cases := []struct{ legacy, modern, value string }{
		{"SERVER_ADDRESS", "MEMQL_SERVER_ADDRESS", "0.0.0.0:8085"},
		{"SERVER_ALLOWED_ORIGINS", "MEMQL_SERVER_ALLOWED_ORIGINS", "https://example.test"},
		{"SERVER_READ_TIMEOUT_MS", "MEMQL_SERVER_READ_TIMEOUT_MS", "1500"},
		{"SERVER_READ_HEADER_TIMEOUT_MS", "MEMQL_SERVER_READ_HEADER_TIMEOUT_MS", "500"},
		{"SERVER_WRITE_TIMEOUT_MS", "MEMQL_SERVER_WRITE_TIMEOUT_MS", "1500"},
		{"SERVER_IDLE_TIMEOUT_MS", "MEMQL_SERVER_IDLE_TIMEOUT_MS", "6000"},
		{"SERVER_SHUTDOWN_TIMEOUT_MS", "MEMQL_SERVER_SHUTDOWN_TIMEOUT_MS", "500"},
		{"SERVER_CAPABILITIES_LOGGING_LOG_LEVEL", "MEMQL_SERVER_CAPABILITIES_LOGGING_LOG_LEVEL", "debug"},
		{"SERVER_LOGGER_LEVEL", "MEMQL_SERVER_LOGGER_LEVEL", "warn"},
		{"SERVER_LOG_LEVEL", "MEMQL_SERVER_LOG_LEVEL", "error"},
		{"SERVICE_NAME", "MEMQL_SERVICE_NAME", "memQL"},
		{"SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL", "MEMQL_SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL", "debug"},
		{"SERVICE_LOGGER_LEVEL", "MEMQL_SERVICE_LOGGER_LEVEL", "warn"},
		{"SERVICE_LOG_LEVEL", "MEMQL_SERVICE_LOG_LEVEL", "error"},
		// The voice-agent's five (memql#3834). Same vintage, found by the same
		// kind of blind spot: an injected getter rather than a struct field.
		{"ANAM_DEFAULT_PERSONA_ID", "MEMQL_ANAM_DEFAULT_PERSONA_ID", "persona-1"},
		{"ANAM_DEFAULT_AVATAR_ID", "MEMQL_ANAM_DEFAULT_AVATAR_ID", "avatar-1"},
		{"ANAM_DEFAULT_PERSONA_NAME", "MEMQL_ANAM_DEFAULT_PERSONA_NAME", "Assistant"},
		{"POLYPHON_VOICE_LANGUAGE", "MEMQL_POLYPHON_VOICE_LANGUAGE", "en"},
		{"VOICE_AGENT_LOG_LEVEL", "MEMQL_VOICE_AGENT_LOG_LEVEL", "DEBUG"},
	}

	for _, tc := range cases {
		t.Run(tc.legacy, func(t *testing.T) {
			// Set the LEGACY name only, and make sure the modern one starts
			// unset -- the shim is set-if-absent, so a leaked value from the
			// environment this test runs in would make it pass without bridging.
			t.Setenv(tc.modern, "")
			if err := os.Unsetenv(tc.modern); err != nil {
				t.Fatalf("unset %s: %v", tc.modern, err)
			}
			t.Setenv(tc.legacy, tc.value)

			ApplyLegacyEnvAliases(nil)

			if got := os.Getenv(tc.modern); got != tc.value {
				t.Errorf("%s did not bridge onto %s: got %q, want %q", tc.legacy, tc.modern, got, tc.value)
			}
		})
	}
}

// New wins over legacy, which is the property that lets a deployment migrate
// its manifests one node at a time without the old value silently overriding
// the new one on a pod that carries both.
func TestServerAliasPrefersModernName(t *testing.T) {
	t.Setenv("SERVER_ADDRESS", "0.0.0.0:1111")
	t.Setenv("MEMQL_SERVER_ADDRESS", "0.0.0.0:2222")

	ApplyLegacyEnvAliases(nil)

	if got := os.Getenv("MEMQL_SERVER_ADDRESS"); got != "0.0.0.0:2222" {
		t.Errorf("legacy value overwrote the modern one: MEMQL_SERVER_ADDRESS = %q, want %q", got, "0.0.0.0:2222")
	}
}
