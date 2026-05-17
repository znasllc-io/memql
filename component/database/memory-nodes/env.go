package memoryNodes

import (
	"strings"

	"github.com/znasllc-io/memql/core/env"
)

type (
	// EnvOptions captures memory nodes configuration derived from environment variables.
	EnvOptions struct {
		ContentIdSalt string
	}

	// EnvKeys defines the environment variable suffixes to read.
	EnvKeys struct {
		ContentIdSalt string
	}

	// EnvLoader describes how to load EnvOptions.
	EnvLoader struct {
		Prefix string
		Keys   EnvKeys
	}
)

const envPrefix = "MEMORY_NODES"

var defaultEnvKeys = EnvKeys{
	ContentIdSalt: "ZNASLLC_LAB_CONTENTID_SALT",
}

// LoadDefaultEnvOptions loads EnvOptions using the default prefix.
func LoadDefaultEnvOptions() (EnvOptions, error) {
	return LoadEnvOptions(EnvLoader{Prefix: envPrefix})
}

// LoadEnvOptions resolves EnvOptions from environment variables.
func LoadEnvOptions(loader EnvLoader) (EnvOptions, error) {
	keys := mergeEnvKeys(loader.Keys)
	prefix := normalizeEnvPrefix(loader.Prefix)
	reader := env.NewEnvReader(prefix)

	opts := EnvOptions{}

	if salt, ok := reader.String(keys.ContentIdSalt); ok {
		opts.ContentIdSalt = salt
	}

	return opts, nil
}

func mergeEnvKeys(keys EnvKeys) EnvKeys {
	merged := defaultEnvKeys

	if trimmed := strings.TrimSpace(keys.ContentIdSalt); trimmed != "" {
		merged.ContentIdSalt = trimmed
	}

	return merged
}

func normalizeEnvPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		trimmed = envPrefix
	}

	if !strings.HasSuffix(trimmed, "_") {
		trimmed += "_"
	}
	return trimmed
}
