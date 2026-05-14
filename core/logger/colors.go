package logger

import (
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/core/env"
)

const colorReset = "\033[0m"

var (
	colorResetBytes = []byte(colorReset)
	colorLookup     = map[string]string{
		"black":         "\033[30m",
		"red":           "\033[31m",
		"green":         "\033[32m",
		"yellow":        "\033[33m",
		"blue":          "\033[34m",
		"magenta":       "\033[35m",
		"cyan":          "\033[36m",
		"white":         "\033[37m",
		"gray":          "\033[90m",
		"grey":          "\033[90m",
		"silver":        "\033[90m",
		"brightred":     "\033[91m",
		"brightgreen":   "\033[92m",
		"brightyellow":  "\033[93m",
		"brightblue":    "\033[94m",
		"brightmagenta": "\033[95m",
		"fuchsia":       "\033[95m",
		"brightcyan":    "\033[96m",
		"brightwhite":   "\033[97m",
		"orange":        "\033[38;5;208m",
		"pink":          "\033[38;5;205m",
		"purple":        "\033[38;5;93m",
		"teal":          "\033[38;5;30m",
		"lime":          "\033[38;5;118m",
		"indigo":        "\033[38;5;56m",
		"gold":          "\033[38;5;220m",
		"violet":        "\033[38;5;129m",
		"maroon":        "\033[38;5;52m",
		"navy":          "\033[38;5;17m",
		"olive":         "\033[38;5;58m",
		"coral":         "\033[38;5;203m",
	}
)

type colorizingWriter struct {
	base   io.Writer
	prefix []byte
	mu     sync.Mutex
}

func (w *colorizingWriter) Write(p []byte) (int, error) {
	if len(w.prefix) == 0 {
		return w.base.Write(p)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.base.Write(w.prefix); err != nil {
		return 0, err
	}

	n, err := w.base.Write(p)
	if err != nil {
		return n, err
	}

	if _, err := w.base.Write(colorResetBytes); err != nil {
		return n, err
	}

	return len(p), nil
}

// ColorizeWriterForComponent returns a writer that applies the configured color for the given component.
func ColorizeWriterForComponent(base io.Writer, component common.ComponentName) io.Writer {
	if base == nil {
		return nil
	}

	if !componentColorEnabled(component) {
		return ensureANSISequenceStrippedWriter(base)
	}

	colorName := lookupComponentColor(component)
	if colorName == "" {
		return ensureANSISequenceStrippedWriter(base)
	}

	colorCode, ok := lookupColor(strings.ToLower(colorName))
	if !ok {
		return ensureANSISequenceStrippedWriter(base)
	}

	return &colorizingWriter{
		base:   base,
		prefix: []byte(colorCode),
	}
}

func componentEnvPrefixes(component common.ComponentName) []string {
	base := env.EnvPrefixForComponent(component)
	if strings.TrimSpace(base) == "" {
		return nil
	}

	seen := make(map[string]struct{})
	var prefixes []string

	add := func(prefix string) {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			return
		}
		if _, ok := seen[prefix]; ok {
			return
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}

	add(base)

	for _, token := range strings.Split(base, "_") {
		add(token)
	}

	return prefixes
}

func lookupColor(name string) (string, bool) {
	if name == "" {
		return "", false
	}

	normalized := normalizeColorName(name)
	code, ok := colorLookup[normalized]
	return code, ok
}

func normalizeColorName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return name
	}

	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func componentColorEnvVars(component common.ComponentName) []string {
	prefixes := componentEnvPrefixes(component)
	if len(prefixes) == 0 {
		return nil
	}

	var (
		vars []string
		seen = make(map[string]struct{}, len(prefixes)*2)
	)

	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		vars = append(vars, key)
	}

	for _, prefix := range prefixes {
		add(prefix + "_CAPABILITIES_LOGGING_LOG_COLOR")
	}

	for _, prefix := range prefixes {
		if prefix == "CACHE" {
			continue
		}
		add(prefix + "_LOG_COLOR")
	}

	return vars
}

func lookupComponentColor(component common.ComponentName) string {
	for _, envVar := range componentColorEnvVars(component) {
		if value := strings.TrimSpace(os.Getenv(envVar)); value != "" {
			return value
		}
	}
	return ""
}

type ansiSequenceStrippingWriter struct {
	base io.Writer
}

func ensureANSISequenceStrippedWriter(base io.Writer) io.Writer {
	if base == nil {
		return nil
	}

	if existing, ok := base.(*ansiSequenceStrippingWriter); ok {
		return existing
	}

	return &ansiSequenceStrippingWriter{base: base}
}

func (w *ansiSequenceStrippingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	clean := stripANSISequences(p)
	if len(clean) == len(p) {
		return w.base.Write(p)
	}

	n, err := w.base.Write(clean)
	if err != nil {
		return n, err
	}

	return len(p), nil
}

func stripANSISequences(p []byte) []byte {
	var (
		result = make([]byte, 0, len(p))
		i      int
	)

	for i < len(p) {
		if p[i] != '\x1b' {
			result = append(result, p[i])
			i++
			continue
		}

		j := i + 1
		if j < len(p) && p[j] == '[' {
			j++
			for j < len(p) {
				if p[j] >= '@' && p[j] <= '~' {
					j++
					break
				}
				j++
			}
			i = j
			continue
		}

		i++
	}

	return result
}

func componentColorEnabled(component common.ComponentName) bool {
	prefixes := componentEnvPrefixes(component)
	if len(prefixes) == 0 {
		return true
	}

	for _, prefix := range prefixes {
		key := strings.TrimSpace(prefix + "_CAPABILITIES_LOGGING_LOG_COLOR_ENABLED")
		if key == "" {
			continue
		}

		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}

		enabled, err := strconv.ParseBool(value)
		if err != nil {
			continue
		}

		if !enabled {
			return false
		}
	}

	return true
}
