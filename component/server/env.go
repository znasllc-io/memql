package server

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/env"
)

type (
	ServerEnvOptions struct {
		Address             string
		ReadTimeoutMs       *int
		ReadHeaderTimeoutMs *int
		WriteTimeoutMs      *int
		IdleTimeoutMs       *int
		ShutdownTimeoutMs   *int
		LoggerLevel         string
		AllowedOrigins      []string
	}

	ServerEnvKeys struct {
		Address             string
		ReadTimeoutMs       string
		ReadHeaderTimeoutMs string
		WriteTimeoutMs      string
		IdleTimeoutMs       string
		ShutdownTimeoutMs   string
		LoggerLevel         string
		AllowedOrigins      string
	}

	ServerEnvLoader struct {
		Prefix string
		Keys   ServerEnvKeys
	}
)

const (
	serverEnvPrefix              = "SERVER"
	serverLoggerLevelKey         = "CAPABILITIES_LOGGING_LOG_LEVEL"
	serverLegacyLoggerLevelKey   = "LOGGER_LEVEL"
	serverFallbackLoggerLevelKey = "LOG_LEVEL"
)

var (
	defaultServerEnvKeys = ServerEnvKeys{
		Address:             "ADDRESS",
		ReadTimeoutMs:       "READ_TIMEOUT_MS",
		ReadHeaderTimeoutMs: "READ_HEADER_TIMEOUT_MS",
		WriteTimeoutMs:      "WRITE_TIMEOUT_MS",
		IdleTimeoutMs:       "IDLE_TIMEOUT_MS",
		ShutdownTimeoutMs:   "SHUTDOWN_TIMEOUT_MS",
		LoggerLevel:         serverLoggerLevelKey,
		AllowedOrigins:      "ALLOWED_ORIGINS",
	}
)

func DefaultServerEnvKeys() ServerEnvKeys {
	return defaultServerEnvKeys
}

func LoadServerEnvOptions(loader ServerEnvLoader) (ServerEnvOptions, error) {
	keys := loader.Keys

	if keys == (ServerEnvKeys{}) {
		keys = defaultServerEnvKeys
	}

	reader := env.NewEnvReader(loader.Prefix)

	var opts ServerEnvOptions

	if value, ok := reader.String(keys.Address); ok {
		opts.Address = value
	}

	readTimeout, err := reader.OptionalInt(keys.ReadTimeoutMs)

	if err != nil {
		return ServerEnvOptions{}, err
	}

	opts.ReadTimeoutMs = readTimeout

	readHeaderTimeout, err := reader.OptionalInt(keys.ReadHeaderTimeoutMs)

	if err != nil {
		return ServerEnvOptions{}, err
	}

	opts.ReadHeaderTimeoutMs = readHeaderTimeout

	writeTimeout, err := reader.OptionalInt(keys.WriteTimeoutMs)

	if err != nil {
		return ServerEnvOptions{}, err
	}

	opts.WriteTimeoutMs = writeTimeout

	idleTimeout, err := reader.OptionalInt(keys.IdleTimeoutMs)

	if err != nil {
		return ServerEnvOptions{}, err
	}

	opts.IdleTimeoutMs = idleTimeout

	shutdownTimeout, err := reader.OptionalInt(keys.ShutdownTimeoutMs)

	if err != nil {
		return ServerEnvOptions{}, err
	}

	opts.ShutdownTimeoutMs = shutdownTimeout

	if value, ok := readLoggerLevel(reader, keys.LoggerLevel); ok {
		opts.LoggerLevel = value
	}

	if value, ok := reader.String(keys.AllowedOrigins); ok {
		opts.AllowedOrigins = splitList(value)
	}

	if len(opts.AllowedOrigins) == 0 {
		opts.AllowedOrigins = []string{"*"}
	}

	return opts, nil
}

func EnvOptionsToArgs(opts ServerEnvOptions) ([]ServerArg, error) {
	var args []ServerArg

	if trimmed := strings.TrimSpace(opts.Address); trimmed != "" {
		args = append(args, WithAddress(trimmed))
	}

	if opts.ReadTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.ReadTimeoutMs)
		if err != nil {
			return nil, fmt.Errorf("read timeout: %w", err)
		}
		args = append(args, WithReadTimeout(duration))
	}

	if opts.ReadHeaderTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.ReadHeaderTimeoutMs)
		if err != nil {
			return nil, fmt.Errorf("read header timeout: %w", err)
		}
		args = append(args, WithReadHeaderTimeout(duration))
	}

	if opts.WriteTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.WriteTimeoutMs)
		if err != nil {
			return nil, fmt.Errorf("write timeout: %w", err)
		}
		args = append(args, WithWriteTimeout(duration))
	}

	if opts.IdleTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.IdleTimeoutMs)
		if err != nil {
			return nil, fmt.Errorf("idle timeout: %w", err)
		}
		args = append(args, WithIdleTimeout(duration))
	}

	if opts.ShutdownTimeoutMs != nil {
		duration, err := durationFromMillis(*opts.ShutdownTimeoutMs)
		if err != nil {
			return nil, fmt.Errorf("shutdown timeout: %w", err)
		}
		args = append(args, WithShutdownTimeout(duration))
	}

	if levelName := strings.TrimSpace(opts.LoggerLevel); levelName != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToLower(levelName))); err != nil {
			return nil, fmt.Errorf("parse logger level: %w", err)
		}
		args = append(args, WithLoggerLevel(level))
	}

	if len(opts.AllowedOrigins) == 0 {
		opts.AllowedOrigins = []string{"*"}
	}

	args = append(args, WithAllowedOrigins(opts.AllowedOrigins...))

	return args, nil
}

func durationFromMillis(ms int) (time.Duration, error) {
	if ms < 0 {
		return 0, fmt.Errorf("milliseconds cannot be negative: %d", ms)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func readLoggerLevel(reader env.EnvReader, key string) (string, bool) {
	candidates := []string{
		key,
		defaultServerEnvKeys.LoggerLevel,
		serverLegacyLoggerLevelKey,
		serverFallbackLoggerLevelKey,
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

func splitList(raw string) []string {
	values := strings.Split(raw, ",")
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
