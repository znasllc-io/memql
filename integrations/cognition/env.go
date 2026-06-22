package cognition

import (
	"strings"

	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/integrations"
)

// CognitionEnvKeys defines the environment variable keys for Cognition logging configuration.
// Note: All other configuration (AI enabled, provider, history limit) comes from
// v1:platform:partitionVariable concept records, resolved at runtime via VariableResolver.
type CognitionEnvKeys struct {
	LoggerLevel string
}

// CognitionEnvOptions holds environment-derived Cognition configuration.
// Only logging configuration uses environment variables.
type CognitionEnvOptions struct {
	// LoggerLevel sets the log level for the Cognition component.
	LoggerLevel string

	// BaseOptions contains base integration options.
	BaseOptions integrations.IntegrationEnvOptions
}

// CognitionEnvLoader loads Cognition configuration from environment variables.
type CognitionEnvLoader struct {
	Prefix string
	Keys   CognitionEnvKeys
}

var defaultCognitionEnvKeys = CognitionEnvKeys{
	LoggerLevel: "CAPABILITIES_LOGGING_LOG_LEVEL",
}

// DefaultCognitionEnvKeys returns the default environment variable keys.
func DefaultCognitionEnvKeys() CognitionEnvKeys {
	return defaultCognitionEnvKeys
}

// LoadCognitionEnvOptions loads Cognition options from environment variables.
// Note: Only logging configuration is loaded from environment variables.
// All other configuration comes from v1:platform:partitionVariable concept records.
func LoadCognitionEnvOptions(loader CognitionEnvLoader) (CognitionEnvOptions, error) {
	keys := loader.Keys
	if keys == (CognitionEnvKeys{}) {
		keys = defaultCognitionEnvKeys
	}

	prefix := loader.Prefix
	if prefix == "" {
		prefix = "COGNITION_INTEGRATION"
	}

	reader := env.NewEnvReader(prefix)

	var opts CognitionEnvOptions

	// Logger level (only env var config for this integration)
	if value, ok := reader.String(keys.LoggerLevel); ok {
		opts.LoggerLevel = strings.TrimSpace(value)
	}

	// Load base integration options
	baseOpts, err := integrations.LoadIntegrationEnvOptions(integrations.IntegrationEnvLoader{
		Prefix: prefix,
	})
	if err != nil {
		return CognitionEnvOptions{}, err
	}
	opts.BaseOptions = baseOpts

	return opts, nil
}
