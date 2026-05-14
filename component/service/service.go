package service

import (
	"fmt"
	"strings"

	"github.com/visionarys-io/memql/core/env"
)

type (
	ServiceEnvOptions struct {
		Name        string
		LoggerLevel string
	}

	ServiceEnvKeys struct {
		Name        string
		LoggerLevel string
	}

	ServiceEnvLoader struct {
		Prefix string
		Keys   ServiceEnvKeys
	}
)

const (
	serviceEnvPrefix            = "SERVICE"
	capabilitiesLoggingLevelKey = "CAPABILITIES_LOGGING_LOG_LEVEL"
	legacyLoggerLevelKey        = "LOGGER_LEVEL"
	fallbackLoggerLevelKey      = "LOG_LEVEL"
)

var (
	defaultServiceEnvKeys = ServiceEnvKeys{
		Name:        "NAME",
		LoggerLevel: capabilitiesLoggingLevelKey,
	}
)

func DefaultServiceEnvKeys() ServiceEnvKeys {
	return defaultServiceEnvKeys
}

func LoadDefaultServiceEnvOptions() (ServiceEnvOptions, error) {
	return LoadServiceEnvOptions(ServiceEnvLoader{Prefix: serviceEnvPrefix})
}

func LoadServiceEnvOptions(loader ServiceEnvLoader) (ServiceEnvOptions, error) {
	keys := defaultServiceEnvKeys
	if loader.Keys != (ServiceEnvKeys{}) {
		if trimmed := strings.TrimSpace(loader.Keys.Name); trimmed != "" {
			keys.Name = trimmed
		}
		if trimmed := strings.TrimSpace(loader.Keys.LoggerLevel); trimmed != "" {
			keys.LoggerLevel = trimmed
		}
	}

	normalizedPrefix := normalizePrefix(loader.Prefix)
	reader := env.NewEnvReader(normalizedPrefix)

	var opts ServiceEnvOptions

	if value, ok := reader.String(keys.Name); ok {
		opts.Name = value
	}

	for _, key := range loggerLevelKeys(keys) {
		if value, ok := reader.String(key); ok {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			opts.LoggerLevel = trimmed
			break
		}
	}

	if strings.TrimSpace(opts.Name) == "" {
		requiredKey := normalizedPrefix + strings.TrimSpace(keys.Name)
		return ServiceEnvOptions{}, fmt.Errorf("%s is required", requiredKey)
	}

	return opts, nil
}

func normalizePrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	if trimmed != "" && !strings.HasSuffix(trimmed, "_") {
		trimmed += "_"
	}
	return trimmed
}

func loggerLevelKeys(keys ServiceEnvKeys) []string {
	var (
		candidates []string
		seen       = make(map[string]struct{})
	)
	add := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		if _, ok := seen[trimmed]; ok {
			return
		}
		seen[trimmed] = struct{}{}
		candidates = append(candidates, trimmed)
	}

	add(keys.LoggerLevel)
	add(defaultServiceEnvKeys.LoggerLevel)
	add(legacyLoggerLevelKey)
	add(fallbackLoggerLevelKey)

	return candidates
}
