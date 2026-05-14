package integrations

import (
	"log/slog"
	"strings"

	"github.com/visionarys-io/memql/core/env"
)

// IntegrationEnvKeys defines the environment variable keys for integration configuration.
type IntegrationEnvKeys struct {
	HTTPTimeout        string
	TickerIntervalMs   string
	MaxRetries         string
	RetryIntervalMs    string
	HealthCheckEnabled string
	LoggerLevel        string
}

// IntegrationEnvOptions holds environment-derived configuration.
type IntegrationEnvOptions struct {
	HTTPTimeoutMs      *int
	TickerIntervalMs   *int
	MaxRetries         *int
	RetryIntervalMs    *int
	HealthCheckEnabled *bool
	LoggerLevel        string
}

// IntegrationEnvLoader loads integration configuration from environment variables.
type IntegrationEnvLoader struct {
	Prefix string
	Keys   IntegrationEnvKeys
}

var defaultIntegrationEnvKeys = IntegrationEnvKeys{
	HTTPTimeout:        "HTTP_TIMEOUT_MS",
	TickerIntervalMs:   "TICKER_INTERVAL_MS",
	MaxRetries:         "MAX_RETRIES",
	RetryIntervalMs:    "RETRY_INTERVAL_MS",
	HealthCheckEnabled: "HEALTH_CHECK_ENABLED",
	LoggerLevel:        "CAPABILITIES_LOGGING_LOG_LEVEL",
}

// DefaultIntegrationEnvKeys returns the default environment variable keys.
func DefaultIntegrationEnvKeys() IntegrationEnvKeys {
	return defaultIntegrationEnvKeys
}

// LoadIntegrationEnvOptions loads integration options from environment variables.
func LoadIntegrationEnvOptions(loader IntegrationEnvLoader) (IntegrationEnvOptions, error) {
	keys := loader.Keys
	if keys == (IntegrationEnvKeys{}) {
		keys = defaultIntegrationEnvKeys
	}

	reader := env.NewEnvReader(loader.Prefix)

	var opts IntegrationEnvOptions

	// HTTP timeout
	httpTimeout, err := reader.OptionalInt(keys.HTTPTimeout)
	if err != nil {
		return IntegrationEnvOptions{}, err
	}
	opts.HTTPTimeoutMs = httpTimeout

	// Ticker interval
	tickerInterval, err := reader.OptionalInt(keys.TickerIntervalMs)
	if err != nil {
		return IntegrationEnvOptions{}, err
	}
	opts.TickerIntervalMs = tickerInterval

	// Max retries
	maxRetries, err := reader.OptionalInt(keys.MaxRetries)
	if err != nil {
		return IntegrationEnvOptions{}, err
	}
	opts.MaxRetries = maxRetries

	// Retry interval
	retryInterval, err := reader.OptionalInt(keys.RetryIntervalMs)
	if err != nil {
		return IntegrationEnvOptions{}, err
	}
	opts.RetryIntervalMs = retryInterval

	// Health check enabled
	healthCheck, err := reader.OptionalBool(keys.HealthCheckEnabled)
	if err != nil {
		return IntegrationEnvOptions{}, err
	}
	opts.HealthCheckEnabled = healthCheck

	// Logger level
	if value, ok := readIntegrationLoggerLevel(reader, keys.LoggerLevel); ok {
		opts.LoggerLevel = value
	}

	return opts, nil
}

func readIntegrationLoggerLevel(reader env.EnvReader, key string) (string, bool) {
	candidates := []string{
		key,
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}

		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}

		if value, ok := reader.String(trimmed); ok {
			if trimmedValue := strings.TrimSpace(value); trimmedValue != "" {
				return trimmedValue, true
			}
		}
	}

	return "", false
}

// EnvOptionsToArgs converts environment options to constructor arguments.
func EnvOptionsToArgs(opts IntegrationEnvOptions) ([]IntegrationArg, error) {
	var args []IntegrationArg

	if levelName := strings.TrimSpace(opts.LoggerLevel); levelName != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToLower(levelName))); err != nil {
			return nil, err
		}
		args = append(args, WithLoggerLevel(level))
	}

	if opts.TickerIntervalMs != nil {
		args = append(args, WithTickerInterval(*opts.TickerIntervalMs))
	}

	if opts.MaxRetries != nil {
		args = append(args, WithMaxRetries(*opts.MaxRetries))
	}

	if opts.RetryIntervalMs != nil {
		args = append(args, WithRetryInterval(*opts.RetryIntervalMs))
	}

	if opts.HealthCheckEnabled != nil {
		args = append(args, WithHealthCheck(*opts.HealthCheckEnabled))
	}

	return args, nil
}
