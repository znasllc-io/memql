// Package config provides centralized configuration loading from environment
// variables. It reads all env vars at startup into a typed ConfigSnapshot
// protobuf message and distributes it to components via the bus wiring.
//
// This replaces scattered os.Getenv() calls across the codebase with a
// single source of truth that is loaded once and distributed via channels.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	"github.com/znasllc-io/memql/core/common"
)

const ComponentName = common.ComponentName("config")

// Component loads configuration from environment variables at startup
// and distributes it to other components via the bus ConfigCh channel.
type Component struct {
	logger   *slog.Logger
	snapshot *busv1.ConfigSnapshot
	readyCh  chan struct{}
}

// New creates a Config component by reading all environment variables.
func New(logger *slog.Logger) *Component {
	return &Component{
		logger:   logger,
		snapshot: loadFromEnv(),
		readyCh:  make(chan struct{}),
	}
}

// Snapshot returns the loaded ConfigSnapshot. Available immediately after New().
// This allows startup code to read config before channels are wired.
func (c *Component) Snapshot() *busv1.ConfigSnapshot {
	return c.snapshot
}

// Start is a no-op for the config component since config is loaded in New().
func (c *Component) Start(_ context.Context) {
	select {
	case <-c.readyCh:
	default:
		close(c.readyCh)
	}
}

// Stop is a no-op.
func (c *Component) Stop(_ context.Context) {}

// IsRunning always returns true since config is always available.
func (c *Component) IsRunning() bool { return true }

// Order returns startup priority. Config loads first.
func (c *Component) Order() int { return 1 }

// ComponentName returns the component identifier.
func (c *Component) ComponentName() common.ComponentName { return ComponentName }

// Ready returns a channel closed when the component is ready.
func (c *Component) Ready() <-chan struct{} { return c.readyCh }

// loadFromEnv reads all known environment variables into a ConfigSnapshot.
func loadFromEnv() *busv1.ConfigSnapshot {
	return &busv1.ConfigSnapshot{
		// Database
		DbDsn: envStr("MEMORY_NODES_DATABASE_DSN"),

		// Engine
		EngineStepCacheEnabled: envBool("MEMQL_STEP_CACHE_ENABLED"),

		// AI
		SiOpenaiApiKey:    envStr("MEMQL_SI_OPENAI_API_KEY"),
		SiOpenaiProjectId: envStr("MEMQL_SI_OPENAI_PROJECT_ID"),
		SiDefaultProvider: envStr("MEMQL_DEFAULT_PROVIDER"),

		// Server
		GrpcAddress:    envStr("MEMQL_GRPC_ADDRESS"),
		HttpPublicPath: envStr("MEMQL_PUBLIC_PATH"),

		// STT
		SttProvider: envStr("MEMQL_STT_PROVIDER"),

		// Auth — IDENTITY_VERIFIER_BASE_URL gates whether bff/voice/etc.
		// run with auth or fall through to the dev no-auth identity.
		// The Jumpcloud* fields on ConfigSnapshot are inert; they
		// remain in the generated proto for backwards-compat with old
		// snapshots and are not populated.
		AuthEnabled:       envStr("IDENTITY_VERIFIER_BASE_URL") != "",
		DelegationEnabled: envBool("MEMQL_DELEGATION_ENABLED"),

		// Polyphon
		PolyphonBridgeAgentUrl: envStr("POLYPHON_BRIDGE_AGENT_URL"),
		PolyphonVoiceProvider:  envStr("POLYPHON_VOICE_PROVIDER"),
		PolyphonOpenaiAsrModel: envStr("POLYPHON_OPENAI_ASR_MODEL"),
		PolyphonOpenaiTtsModel: envStr("POLYPHON_OPENAI_TTS_MODEL"),
		PolyphonOpenaiTtsVoice: envStr("POLYPHON_OPENAI_TTS_VOICE"),

		// Node
		NodeType:           envStr("MEMQL_NODE_TYPE"),
		NodeId:             envStr("MEMQL_NODE_ID"),
		NodeAddress:        envStr("MEMQL_NODE_ADDRESS"),
		NodeParentAddress:  envStr("MEMQL_PARENT_ADDRESS"),
		NodeLabels:         envStr("MEMQL_NODE_LABELS"),
		NodeServiceAddress: envStr("MEMQL_NODE_SERVICE_ADDRESS"),

		// Object storage is Azure Blob (memql#801); the agent node reads
		// MEMQL_AZURE_BLOB_CONTAINER + MEMQL_AZURE_STORAGE_CONNECTION_STRING
		// directly in app/transport_agent.go. (The legacy ConfigSnapshot
		// GcsBucket field is unused and left unset.)

		// Runtime-mutable
		DemoMode: envBool("MEMQL_DEMO_MODE"),

		// Version
		Version: envStr("VERSION"),

		// Partition is no longer env-driven. Defaults to "default" in
		// the engine; per-request override comes via the gRPC
		// envelope. Field stays on the proto for wire compatibility.
		Partition: "",

		// Cognition guardrails. Fit-score threshold; defaults to 0.4
		// when unset or zero.
		CognitionFitThreshold: envFloat("MEMQL_COGNITION_FIT_THRESHOLD", 0.4),

		// Feature flags
		FeatureFlags: map[string]string{},
	}
}

// envFloat reads an environment variable as a float64, returning the
// default when unset, empty, or unparseable.
func envFloat(key string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var v float64
	if _, err := fmt.Sscanf(raw, "%f", &v); err != nil {
		return def
	}
	return v
}

// envStr reads and trims an environment variable.
func envStr(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// envBool reads an environment variable as a boolean.
func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "true" || v == "1" || v == "yes"
}
