package logger

import (
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

func New(componentName common.ComponentName, writer io.Writer, level slog.Level) *slog.Logger {
	if !componentLoggingEnabled(componentName) {
		writer = io.Discard
	}

	if writer != nil {
		writer = NewOrderedJSONWriter(writer)
	}

	writer = ColorizeWriterForComponent(writer, componentName)

	// Wrap the JSON handler with the redacting handler so attribute
	// keys matching ShouldRedact land as "<redacted>" in the
	// output. Defense-in-depth: catches the common mistake of
	// passing a sensitive value as a typed attr (e.g.
	// slog.Info("login", "password", pw)) without relying on every
	// caller remembering the discipline. See core/logger/redact.go
	// for the predicate.
	//
	// The chain is redactingHandler -> fanout(JSONHandler, storeHandler)
	// (epic memql#4893). The store handler forwards each line to the
	// process's registered logger.Sink -- component/logstore, the log_line
	// hypertable -- and it sits INSIDE the redactor on purpose: a stored
	// attribute is exactly what the console would have printed, so a
	// `token` reaches the store as "<redacted>". The redactor must stay
	// OUTERMOST for that to hold; see store.go.
	base := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level})
	return slog.New(NewRedactingHandler(newFanoutHandler(base, newStoreHandler()))).With("component", componentName)
}

func componentLoggingEnabled(component common.ComponentName) bool {
	for _, key := range componentLoggingEnvKeys(component) {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}

		enabled, err := strconv.ParseBool(value)
		if err != nil {
			continue
		}

		return enabled
	}

	return true
}

func componentLoggingEnvKeys(component common.ComponentName) []string {
	prefixes := componentEnvPrefixes(component)
	if len(prefixes) == 0 {
		return nil
	}

	keys := make([]string, 0, len(prefixes))
	seen := make(map[string]struct{}, len(prefixes))

	for _, prefix := range prefixes {
		key := strings.TrimSpace(prefix + "_CAPABILITIES_LOGGING")
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	return keys
}

func FatalWithLogger(logger *slog.Logger, message string, args ...any) {
	logger.Error(message, args...)
	os.Exit(1)
}

func Fatal(message string, args ...any) {
	FatalWithLogger(slog.Default(), message, args...)
}

func LoggingEnabled(component common.ComponentName) bool {
	return componentLoggingEnabled(component)
}
