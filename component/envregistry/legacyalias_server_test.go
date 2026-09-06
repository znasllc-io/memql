package envregistry

import (
	"os"
	"testing"
)

// The SERVER_* / SERVICE_* bridge (memql#3892), plus the two POLYPHON_ aliases
// that outlived the voice node type.
//
// It used to carry the voice-agent's five pre-convention names (memql#3834) as
// its second half. Epic memql#4988 deleted the voice node type and every
// variable those five pointed at, so an alias to any of them now leads
// nowhere; they are gone from LegacyAliases and from here.
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
		{"SERVICE_NAME", "MEMQL_SERVICE_NAME", "MemQL"},
		{"SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL", "MEMQL_SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL", "debug"},
		{"SERVICE_LOGGER_LEVEL", "MEMQL_SERVICE_LOGGER_LEVEL", "warn"},
		{"SERVICE_LOG_LEVEL", "MEMQL_SERVICE_LOG_LEVEL", "error"},
		// The two POLYPHON_ aliases epic memql#4988 kept. They are Epic 7.3
		// entries (memql#2106) rather than memql#3834's vintage, and they
		// survived because their targets did: both configure the OpenAI
		// Realtime ASR session behind AiTranscribeStream*, which the agent
		// node serves. Asserted here because an operator who set the bare
		// spelling years ago still has it set.
		{"POLYPHON_OPENAI_ASR_MODEL", "MEMQL_POLYPHON_OPENAI_ASR_MODEL", "gpt-4o-transcribe"},
		{"POLYPHON_OPENAI_VAD_SILENCE_MS", "MEMQL_POLYPHON_OPENAI_VAD_SILENCE_MS", "600"},
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
