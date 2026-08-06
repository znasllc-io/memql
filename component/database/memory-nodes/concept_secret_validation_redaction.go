package memoryNodes

import (
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// redactedSchemaMessage replaces the message of a validation error whose
// instance is a @secret field.
//
// The whole message is replaced rather than the value surgically removed. The
// jsonschema messages are free prose that differ per keyword ("must be >=
// 100000 but found 4242", "'sk-live-…' is not valid 'date-time'"), so any
// attempt to excise just the value would be a per-keyword parser -- another
// definition of "where the value sits in this string" that drifts the moment
// the library rewords one. Replacing wholesale cannot leak by omission.
//
// Nothing diagnostic is lost that the error did not already carry elsewhere:
// ValidationError.Error() prints the KeywordLocation beside the message, so
// which constraint failed is still on the wire -- only the value is gone.
const redactedSchemaMessage = "<redacted> -- the field is declared @secret, so the rejected value " +
	"is withheld; the keyword location above names which constraint failed (memql#3112)"

// redactSecretsInValidationError rewrites, IN PLACE, the message of every node
// in a jsonschema validation-error tree whose instance location points at a
// field the concept declares @secret.
//
// # Why this exists
//
// Concept payload validation enforces @minimum / @maximum / @format / @minLength
// declared on the CONCEPT, and santhosh-tekuri/jsonschema interpolates the
// offending INSTANCE VALUE into its message. Concept.Create wraps that verbatim
// as "concept payload validation failed: %w", so a rejected secret value
// travelled into the caller's error and any log that recorded it.
//
// memql#3036 added redaction to the function-args validator, which covers only
// constraints the mutation's ARGS BLOCK declares. A constraint declared on the
// concept and not mirrored in the args block is validated ONLY here, so that
// redaction was bypassed entirely -- no automation and no matching arg name
// required. That made this the largest of the surfaces #3036 left uncovered
// (memql#3112).
//
// # Why the whole tree, not just the first leaf
//
// ValidationError.Error() renders only the FIRST leaf, so redacting that one
// would satisfy a test that prints the error. But the tree is exported and
// mutable: callers can walk Causes, and GoString() renders a node directly.
// Redacting only what Error() happens to print would be a redaction that holds
// exactly as long as nobody inspects the structure.
//
// # Matching
//
// SecretFields is top-level, so the FIRST segment of the instance location is
// what is compared. A nested instance location (`/apiKey/inner`) under a secret
// top-level field is redacted too: the value being quoted is still part of that
// field. Being broad here is the safe direction -- the failure mode of over-
// redaction is a less specific error, and of under-redaction is a leaked
// credential.
//
// Returns err unchanged when the concept declares no secret fields, or when err
// is not a jsonschema validation error. Non-secret fields' messages are
// untouched, byte for byte.
func (c *Concept) redactSecretsInValidationError(err error) error {
	if err == nil || c == nil {
		return err
	}
	fields := c.SecretFields()
	if len(fields) == 0 {
		return err
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err
	}
	secret := make(map[string]bool, len(fields))
	for _, f := range fields {
		secret[f] = true
	}
	redactValidationNode(ve, secret)
	return ve
}

// redactValidationNode walks the tree depth-first, rewriting the message of
// every node whose instance location names a secret field.
func redactValidationNode(ve *jsonschema.ValidationError, secret map[string]bool) {
	if ve == nil {
		return
	}
	if ve.Message != "" && secret[topLevelInstanceField(ve.InstanceLocation)] {
		ve.Message = redactedSchemaMessage
	}
	for _, cause := range ve.Causes {
		redactValidationNode(cause, secret)
	}
}

// topLevelInstanceField extracts the first path segment of a JSON-pointer-style
// instance location: "/apiKey" -> "apiKey", "/apiKey/inner" -> "apiKey", "" -> "".
//
// The root location is "" (the whole instance), which matches no field name and
// so is never redacted -- correct, because a root-level message ("missing
// properties: 'x'") names fields rather than values.
func topLevelInstanceField(loc string) string {
	loc = strings.TrimPrefix(loc, "/")
	if loc == "" {
		return ""
	}
	if i := strings.IndexByte(loc, '/'); i >= 0 {
		loc = loc[:i]
	}
	// JSON pointer escapes: ~1 is '/', ~0 is '~'. Unescape in that order.
	loc = strings.ReplaceAll(loc, "~1", "/")
	loc = strings.ReplaceAll(loc, "~0", "~")
	return loc
}
