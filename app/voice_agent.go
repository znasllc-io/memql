package app

import (
	"os"
	"strings"
)

// voiceAgentSharedToken reads MEMQL_VOICE_AGENT_SHARED_TOKEN at startup.
// Empty disables the voice-agent admit path entirely -- the
// interceptor will reject any incoming mql_va_<...> bearer when the
// configured value is empty.
//
// Phase 2 of Initiative C ships this as a single shared secret. A
// follow-up will swap to identity-issued service-account tokens; this
// helper goes away then.
func voiceAgentSharedToken() string {
	return strings.TrimSpace(os.Getenv("MEMQL_VOICE_AGENT_SHARED_TOKEN"))
}
