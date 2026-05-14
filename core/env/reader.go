package env

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type EnvReader struct {
	prefix string
	lookup func(string) (string, bool)
}

func NewEnvReader(prefix string) EnvReader {
	normalized := strings.TrimSpace(prefix)
	if normalized != "" && !strings.HasSuffix(normalized, "_") {
		normalized += "_"
	}

	return EnvReader{
		prefix: normalized,
		lookup: os.LookupEnv,
	}
}

func (r EnvReader) WithLookup(fn func(string) (string, bool)) EnvReader {
	r.lookup = fn
	return r
}

func (r EnvReader) String(key string) (string, bool) {
	if strings.TrimSpace(key) == "" {
		return "", false
	}

	fullKey := r.fullKey(key)
	value, ok := r.lookup(fullKey)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func (r EnvReader) OptionalBool(key string) (*bool, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("environment key cannot be empty")
	}

	fullKey := r.fullKey(key)
	value, ok := r.lookup(fullKey)
	if !ok {
		return nil, nil
	}

	trimmed := strings.TrimSpace(value)
	parsed, err := strconv.ParseBool(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fullKey, err)
	}

	return &parsed, nil
}

func (r EnvReader) OptionalInt(key string) (*int, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("environment key cannot be empty")
	}

	fullKey := r.fullKey(key)
	value, ok := r.lookup(fullKey)
	if !ok {
		return nil, nil
	}

	trimmed := strings.TrimSpace(value)
	parsed, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fullKey, err)
	}

	return &parsed, nil
}

func (r EnvReader) fullKey(key string) string {
	trimmedKey := strings.TrimSpace(key)
	return r.prefix + trimmedKey
}
