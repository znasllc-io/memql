package polyphon

import (
	"os"
	"strings"
)

// Config holds the Polyphon LiveKit transport configuration.
// The score engine is always active. This config only controls
// the LiveKit transport layer for multi-agent voice rooms.
type Config struct {
	// LiveKitURL is the LiveKit server WebSocket URL (used for server-side connections).
	// Set via POLYPHON_LIVEKIT_URL environment variable.
	LiveKitURL string

	// LiveKitPublicURL is the browser-reachable LiveKit WebSocket URL.
	// In Docker, the internal hostname (e.g. ws://livekit:7880) is not reachable
	// from the browser, so this provides the external URL (e.g. ws://localhost:7880).
	// Falls back to LiveKitURL if not set.
	// Set via POLYPHON_LIVEKIT_PUBLIC_URL environment variable.
	LiveKitPublicURL string

	// LiveKitAPIKey is the LiveKit API key.
	// Set via POLYPHON_LIVEKIT_API_KEY environment variable.
	LiveKitAPIKey string

	// LiveKitAPISecret is the LiveKit API secret.
	// Set via POLYPHON_LIVEKIT_API_SECRET environment variable.
	LiveKitAPISecret string

	// Scoring weights override (optional).
	Weights *ScoringWeights

	// Turn policy override (optional).
	Policy *TurnPolicyConfig
}

// ConfigFromEnv loads Polyphon configuration from environment variables.
func ConfigFromEnv() Config {
	return Config{
		LiveKitURL:       envString("POLYPHON_LIVEKIT_URL", ""),
		LiveKitPublicURL: envString("POLYPHON_LIVEKIT_PUBLIC_URL", ""),
		LiveKitAPIKey:    envString("POLYPHON_LIVEKIT_API_KEY", ""),
		LiveKitAPISecret: envString("POLYPHON_LIVEKIT_API_SECRET", ""),
	}
}

// LiveKitConfigured returns true when all LiveKit credentials are present.
// The transport layer (room tokens, voice rooms) is only available when
// LiveKit URL (must start with ws:// or wss://), API key, and API secret are all set.
func (c Config) LiveKitConfigured() bool {
	return (strings.HasPrefix(c.LiveKitURL, "ws://") || strings.HasPrefix(c.LiveKitURL, "wss://")) &&
		c.LiveKitAPIKey != "" && c.LiveKitAPISecret != ""
}

func envString(key, defaultVal string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	return v
}
