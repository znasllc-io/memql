package logger

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// RedactedPlaceholder is the string emitted in place of a sensitive
// value. Stable so log shippers / regex consumers can spot the
// pattern.
const RedactedPlaceholder = "<redacted>"

// secretNamePattern matches attribute keys that conventionally
// carry sensitive values. Mirrors `component/observe/helper.go`'s
// secretNamePattern -- duplicated here intentionally so this
// package stays free of an observe dependency (the observe runtime
// imports logger, not the other way around).
var secretNamePattern = regexp.MustCompile(`(?i)(pass|token|secret|key|auth|credential)`)

// explicitDenylist is the case-insensitive set of attribute keys
// that are ALWAYS redacted, even when secretNamePattern doesn't
// match. Covers wire-protocol / AI fields whose names don't carry
// one of the usual secret-marker roots:
//
//   - authorization / bearer / cookie / set-cookie -- HTTP credential headers
//   - prompt / completion / payload / body         -- AI / wire content that
//                                                     can contain sensitive
//                                                     text the caller shouldn't
//                                                     leak via a debug log.
var explicitDenylist = map[string]struct{}{
	"authorization": {},
	"bearer":        {},
	"cookie":        {},
	"set-cookie":    {},
	"prompt":        {},
	"completion":    {},
	"payload":       {},
	"body":          {},
}

// ShouldRedact returns true when an attribute key should have its
// value replaced with RedactedPlaceholder before emission. Two
// strategies, OR'd:
//
//  1. secretNamePattern (case-insensitive substring on the key).
//  2. The explicit denylist (exact match, case-insensitive).
//
// Exported so callers + tests can probe the same predicate the
// redacting handler uses.
func ShouldRedact(key string) bool {
	if key == "" {
		return false
	}
	if secretNamePattern.MatchString(key) {
		return true
	}
	_, ok := explicitDenylist[strings.ToLower(key)]
	return ok
}

// redactingHandler wraps an inner slog.Handler. Every attribute
// whose key matches ShouldRedact lands at the inner handler with
// its value substituted for RedactedPlaceholder. Groups are walked
// recursively so a denylisted key nested under
// `slog.Group("session", "token", value)` still gets redacted.
//
// Defense-in-depth, not a substitute for not-logging-secrets. A
// caller who hand-builds a JSON string containing a secret and
// passes it as a non-denylisted attr key still leaks; the handler
// only knows about attribute KEYS, not the contents of arbitrary
// values. The point of the wrapper is to backstop the easy mistake
// (`slog.Info("login", "password", pw)`), not to plug every
// theoretical exfiltration path.
type redactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler wraps an existing slog.Handler so every
// attribute key matching ShouldRedact lands as RedactedPlaceholder
// in the inner handler's output. Designed to be the outermost
// handler in a chain so it sees the full attr set every caller
// adds.
func NewRedactingHandler(inner slog.Handler) slog.Handler {
	if inner == nil {
		return nil
	}
	return &redactingHandler{inner: inner}
}

// Enabled defers to the inner handler. The wrapper has no level
// opinion of its own -- it's purely an attribute filter.
func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle redacts attrs and forwards to the inner handler. Record
// time / level / message / PC pass through unchanged; only attrs
// are rewritten.
func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(attr slog.Attr) bool {
		out.AddAttrs(redactAttr(attr))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs returns a handler that pre-binds the (redacted) attrs.
// The inner handler's WithAttrs sees only the already-redacted
// values so the cached attr list never carries a secret.
func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(out)}
}

// WithGroup returns a handler that wraps the inner handler's
// grouped output. Attribute redaction still applies inside the
// group -- the group prefix changes the formatted key but the
// raw key the redactor sees stays the same.
func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr returns a copy of attr with its value replaced by
// RedactedPlaceholder if the key matches ShouldRedact. For Group
// values, recurses so a denylisted key under a non-denylisted
// group key still gets redacted.
func redactAttr(attr slog.Attr) slog.Attr {
	if ShouldRedact(attr.Key) {
		return slog.String(attr.Key, RedactedPlaceholder)
	}
	if attr.Value.Kind() == slog.KindGroup {
		groupAttrs := attr.Value.Group()
		out := make([]slog.Attr, 0, len(groupAttrs))
		for _, ga := range groupAttrs {
			out = append(out, redactAttr(ga))
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(out...)}
	}
	return attr
}
