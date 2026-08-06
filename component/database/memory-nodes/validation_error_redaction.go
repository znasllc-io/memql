package memoryNodes

import (
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// RedactedSchemaMessage replaces the message of a validation error whose
// instance is a field declared @secret.
//
// The whole message is replaced rather than the value surgically removed. The
// jsonschema messages are free prose that differ per keyword ("must be >=
// 100000 but found 4242", "'sk-live-...' is not valid 'date-time'"), so
// excising just the value would need a per-keyword parser -- another definition
// of "where the value sits in this string" that drifts the moment the library
// rewords one. Replacing wholesale cannot leak by omission.
//
// Nothing diagnostic is lost that the error did not carry elsewhere:
// ValidationError renders the KeywordLocation beside the message, so which
// constraint failed is still on the wire -- only the value is gone.
const RedactedSchemaMessage = "<redacted> -- the field is declared @secret, so the rejected " +
	"value is withheld; the keyword location names which constraint failed"

// RedactSecretsInValidationError rewrites, IN PLACE, the message of every node
// in a jsonschema validation-error tree whose instance location names a field
// in secret.
//
// # Why it lives here and is exported
//
// Two validators interpolate a rejected value into a jsonschema message and
// need exactly this walk: concept payload validation (Concept.validate, in this
// package) and the engine's tool-args validator
// (component/memql.validateToolArgs, memql#3117). A second copy would be a
// second definition of "which node carries a secret", and this repo has paid
// for duplicate definitions of one rule repeatedly -- memql#3035 and memql#3099
// are the same lesson about escape sets.
//
// # Why the whole tree, not just the first leaf
//
// ValidationError.Error() renders only the FIRST leaf, so redacting that one
// would satisfy a test that prints the error. But the tree is exported and
// mutable: callers can walk Causes, and GoString() renders a node directly.
// Redacting only what Error() happens to print is a redaction that holds
// exactly as long as nobody inspects the structure.
//
// # Matching
//
// The FIRST segment of the instance location is compared, so a nested location
// (`/apiKey/inner`) under a secret top-level field is redacted too: the value
// being quoted is still part of that field. Being broad here is the safe
// direction -- over-redaction costs a less specific error, under-redaction
// leaks a credential.
//
// Returns err unchanged when secret is empty or err is not a jsonschema
// validation error. Non-secret fields' messages are untouched, byte for byte.
func RedactSecretsInValidationError(err error, secret map[string]bool) error {
	if err == nil || len(secret) == 0 {
		return err
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err
	}
	redactValidationNode(ve, secret)
	return ve
}

func redactValidationNode(ve *jsonschema.ValidationError, secret map[string]bool) {
	if ve == nil {
		return
	}
	if ve.Message != "" && secret[TopLevelInstanceField(ve.InstanceLocation)] {
		ve.Message = RedactedSchemaMessage
	}
	for _, cause := range ve.Causes {
		redactValidationNode(cause, secret)
	}
}

// TopLevelInstanceField extracts the first path segment of a JSON-pointer-style
// instance location: "/apiKey" -> "apiKey", "/apiKey/inner" -> "apiKey".
//
// The root location is "" (the whole instance), which matches no field name and
// so is never redacted -- correct, because a root-level message ("missing
// properties: 'x'") names fields rather than values.
func TopLevelInstanceField(loc string) string {
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
