package memqlws

import (
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/env"
)

type (
	// EnvOptions capture optional overrides supplied by environment variables.
	EnvOptions struct {
		DialTimeoutMs         *int
		WriteTimeoutMs        *int
		MaxConcurrentRequests *int
		MaxMessageBytes       *int
		PingIntervalMs        *int
	}

	// EnvKeys maps logical option names to environment suffixes.
	EnvKeys struct {
		DialTimeoutMs         string
		WriteTimeoutMs        string
		MaxConcurrentRequests string
		MaxMessageBytes       string
		PingIntervalMs        string
	}

	// EnvLoader configures how environment overrides are loaded.
	EnvLoader struct {
		Prefix string
		Keys   EnvKeys
	}
)

const defaultEnvPrefix = "MEMQL_WS"

var defaultEnvKeys = EnvKeys{
	DialTimeoutMs:         "DIAL_TIMEOUT_MS",
	WriteTimeoutMs:        "WRITE_TIMEOUT_MS",
	MaxConcurrentRequests: "MAX_CONCURRENT_REQUESTS",
	MaxMessageBytes:       "MAX_MESSAGE_BYTES",
	PingIntervalMs:        "PING_INTERVAL_MS",
}

// LoadEnvOptions reads optional overrides using the provided loader configuration.
func LoadEnvOptions(loader EnvLoader) (EnvOptions, error) {
	prefix := loader.Prefix
	if strings.TrimSpace(prefix) == "" {
		prefix = defaultEnvPrefix
	}

	keys := loader.Keys
	if keys == (EnvKeys{}) {
		keys = defaultEnvKeys
	}

	reader := env.NewEnvReader(prefix)
	var opts EnvOptions

	dialTimeout, err := reader.OptionalInt(keys.DialTimeoutMs)
	if err != nil {
		return EnvOptions{}, err
	}
	opts.DialTimeoutMs = dialTimeout

	writeTimeout, err := reader.OptionalInt(keys.WriteTimeoutMs)
	if err != nil {
		return EnvOptions{}, err
	}
	opts.WriteTimeoutMs = writeTimeout

	maxConcurrent, err := reader.OptionalInt(keys.MaxConcurrentRequests)
	if err != nil {
		return EnvOptions{}, err
	}
	opts.MaxConcurrentRequests = maxConcurrent

	maxBytes, err := reader.OptionalInt(keys.MaxMessageBytes)
	if err != nil {
		return EnvOptions{}, err
	}
	opts.MaxMessageBytes = maxBytes

	pingInterval, err := reader.OptionalInt(keys.PingIntervalMs)
	if err != nil {
		return EnvOptions{}, err
	}
	opts.PingIntervalMs = pingInterval

	return opts, nil
}

// Apply mutates the supplied Options struct with any override values.
func (opts EnvOptions) Apply(target *Options) error {
	if target == nil {
		return fmt.Errorf("options target must not be nil")
	}

	if opts.DialTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.DialTimeoutMs)
		if err != nil {
			return fmt.Errorf("memql ws dial timeout: %w", err)
		}
		target.DialTimeout = duration
	}

	if opts.WriteTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.WriteTimeoutMs)
		if err != nil {
			return fmt.Errorf("memql ws write timeout: %w", err)
		}
		target.WriteTimeout = duration
	}

	if opts.MaxConcurrentRequests != nil && *opts.MaxConcurrentRequests > 0 {
		target.MaxConcurrentRequests = *opts.MaxConcurrentRequests
	}

	if opts.MaxMessageBytes != nil && *opts.MaxMessageBytes > 0 {
		target.MaxMessageBytes = int64(*opts.MaxMessageBytes)
	}

	if opts.PingIntervalMs != nil {
		duration, err := durationFromMillis(*opts.PingIntervalMs)
		if err != nil {
			return fmt.Errorf("memql ws ping interval: %w", err)
		}
		target.PingInterval = duration
	}

	return nil
}

func durationFromMillis(ms int) (time.Duration, error) {
	if ms < 0 {
		return 0, fmt.Errorf("milliseconds cannot be negative: %d", ms)
	}
	return time.Duration(ms) * time.Millisecond, nil
}
