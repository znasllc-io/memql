package env

import (
	"os"
	"regexp"
	"strings"
)

// envVarPattern matches ${VAR_NAME} syntax for environment variable substitution.
// Variable names must start with a letter or underscore, followed by letters, digits, or underscores.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// SubstituteEnvVars recursively walks through a value and replaces ${VAR_NAME}
// patterns with the corresponding environment variable values.
//
// Supported types:
//   - string: All ${VAR_NAME} patterns are replaced (supports inline substitution)
//   - map[string]any: Each value is recursively processed
//   - map[string]string: Each value is processed for substitution
//   - []any: Each element is recursively processed
//
// If an environment variable is not set, the placeholder remains unchanged.
// This makes it obvious when a required variable is missing.
func SubstituteEnvVars(value any) any {
	if value == nil {
		return nil
	}

	switch v := value.(type) {
	case string:
		return SubstituteEnvVarsInString(v)
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, val := range v {
			result[key] = SubstituteEnvVars(val)
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(v))
		for key, val := range v {
			result[key] = SubstituteEnvVarsInString(val)
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, val := range v {
			result[i] = SubstituteEnvVars(val)
		}
		return result
	default:
		return value
	}
}

// SubstituteEnvVarsInString replaces all ${VAR_NAME} patterns in a string
// with the corresponding environment variable values.
//
// If an environment variable is not set, the placeholder remains unchanged.
// This supports both exact matches ("${VAR}") and inline substitution ("prefix_${VAR}_suffix").
func SubstituteEnvVarsInString(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the variable name from ${VAR_NAME}
		submatch := envVarPattern.FindStringSubmatch(match)
		if len(submatch) < 2 {
			return match
		}
		varName := submatch[1]
		envValue := os.Getenv(varName)
		// If the environment variable is not set, keep the original placeholder
		if envValue == "" {
			return match
		}
		return envValue
	})
}

// SubstituteEnvVarsStrict is like SubstituteEnvVarsInString but returns an error
// if any referenced environment variable is not set.
//
// This is useful for cases where missing variables should cause a hard failure
// rather than silently keeping the placeholder.
func SubstituteEnvVarsStrict(s string) (string, error) {
	var missingVars []string

	result := envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		submatch := envVarPattern.FindStringSubmatch(match)
		if len(submatch) < 2 {
			return match
		}
		varName := submatch[1]
		envValue := os.Getenv(varName)
		if envValue == "" {
			missingVars = append(missingVars, varName)
			return match
		}
		return envValue
	})

	if len(missingVars) > 0 {
		return "", &EnvVarNotSetError{Variables: missingVars}
	}

	return result, nil
}

// SubstituteEnvVarsMap processes a map[string]string, substituting environment
// variables in each value. Returns an error if any referenced variable is not set.
//
// This is a convenience function for auth configurations and similar use cases
// where all values should be fully resolved.
func SubstituteEnvVarsMap(values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return values, nil
	}

	resolved := make(map[string]string, len(values))
	for key, value := range values {
		result, err := SubstituteEnvVarsStrict(value)
		if err != nil {
			return nil, err
		}
		resolved[key] = result
	}

	return resolved, nil
}

// HasEnvVarPlaceholder checks if a string contains any ${VAR_NAME} placeholders.
func HasEnvVarPlaceholder(s string) bool {
	return envVarPattern.MatchString(s)
}

// EnvVarNotSetError is returned when a required environment variable is not set.
type EnvVarNotSetError struct {
	Variables []string
}

func (e *EnvVarNotSetError) Error() string {
	if len(e.Variables) == 1 {
		return "environment variable " + e.Variables[0] + " is not set"
	}
	return "environment variables not set: " + strings.Join(e.Variables, ", ")
}
