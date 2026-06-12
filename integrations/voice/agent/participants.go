package agent

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/integrations/avatarvendor"
)

// participants.go -- human-vs-system participant classification shared by the
// auto-join dispatcher (room discovery) and the room joiner (idle teardown).
// CGO-free so the untagged test suite covers it.

// defaultIdleTeardown is how long a joined room may sit with zero human
// participants before the session tears itself down (reason "inactivity") and
// the dispatcher returns to discovery. Without this, the agent -- itself a
// room participant -- keeps the room alive forever and LiveKit never
// disconnects it, wedging the single-room dispatcher on a dead room (#1378).
const defaultIdleTeardown = 60 * time.Second

// agentIdentitySuffix matches the voice-agent's own room identity: the join
// token is minted with req.GaAgentID (room_voice.go), which the session
// composes as `<spaceId>-ga` (session.go, gaAgentID := spaceID + "-ga") --
// e.g. `v1:cognition:space:<uuid>-ga`. Human identities are cognition
// participant ids (hex shortIds), so a `-ga` suffix cannot collide ('g' is
// not a hex digit).
const agentIdentitySuffix = "-ga"

// agentIdentityPrefix additionally matches an agent-concept-id identity in
// case a future joiner mints with the real agent row id.
const agentIdentityPrefix = "v1:agents:agent:"

// isHumanParticipantIdentity reports whether a room participant identity
// belongs to a human (the SPA / a guest) rather than to system machinery the
// stack itself placed in the room: the voice-agent (the `<spaceId>-ga`
// placeholder identity, including a terminating predecessor pod's ghost
// during a rollout) or an avatar vendor participant (the fixed "avatar-agent"
// identity).
func isHumanParticipantIdentity(identity string) bool {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return false
	}
	if identity == avatarvendor.AvatarParticipantIdentity {
		return false
	}
	if strings.HasSuffix(identity, agentIdentitySuffix) {
		return false
	}
	return !strings.HasPrefix(identity, agentIdentityPrefix)
}

// idleTeardownAfter resolves the zero-humans grace period before the session
// tears down, honoring MEMQL_VOICE_IDLE_TEARDOWN_SECONDS (positive integer)
// and falling back to the default.
func idleTeardownAfter(getenv func(string) string) time.Duration {
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := strings.TrimSpace(getenv("MEMQL_VOICE_IDLE_TEARDOWN_SECONDS"))
	if raw == "" {
		return defaultIdleTeardown
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return defaultIdleTeardown
	}
	return time.Duration(secs) * time.Second
}
