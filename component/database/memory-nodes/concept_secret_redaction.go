package memoryNodes

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// redactedSecretValue is the placeholder a @secret field's value is replaced
// with in an operator-visible validation message.
//
// It is deliberately the SAME literal the memql function-args validator uses
// (redactedArgValue, component/memql/function_validator.go) so one grep finds
// every redaction this engine performs. It is re-declared here rather than
// imported: component/memql already depends on this package, and importing
// back would be an import cycle.
const redactedSecretValue = "<redacted>"

// valueInterpolatingKeyword describes WHERE in a jsonschema v5 message the
// instance value sits, so the value can be cut out without disturbing the
// constraint half of the message.
//
// separator is a literal that jsonschema's format string puts adjacent to the
// instance value; valueFirst says which side of it the value is on.
type valueInterpolatingKeyword struct {
	separator  string
	valueFirst bool
}

// valueInterpolatingKeywords enumerates EVERY jsonschema/v5 keyword whose
// failure message interpolates the INSTANCE VALUE.
//
// Derived by reading the vendored library rather than by guessing: every
// message this library can emit is built by exactly one call to
// validationError(keyword, format, args...) in
//
//	$(go env GOMODCACHE)/github.com/santhosh-tekuri/jsonschema/v5@v5.3.1/schema.go
//
// (the only other site is extension.go:108, reached only by a registered
// custom-keyword Extension -- this engine registers none). Reading all 37 of
// those call sites, the value being validated (`v`, or the `val` derived from
// it) reaches the format string at exactly six:
//
//	schema.go:311  format            "%v is not valid %s"      (val, quote(s.Format))
//	schema.go:598  minimum           "must be >= %v but found %v"
//	schema.go:601  exclusiveMinimum  "must be > %v but found %v"
//	schema.go:604  maximum           "must be <= %v but found %v"
//	schema.go:607  exclusiveMaximum  "must be < %v but found %v"
//	schema.go:611  multipleOf        "%v not multipleOf %v"    (v, f64(s.MultipleOf))
//
// The concept parser emits four of these directly (@minimum -> minimum,
// @maximum -> maximum, @format / the optional-datetime oneOf -> format) and can
// reach the rest through a hand-written schema variant, so all six are handled.
//
// The other 31 sites are NOT in this list, and each was checked individually
// rather than skimmed. They fall into four groups:
//
//   - Schema-side values only, no instance: const ("value must be %#v" prints
//     s.Constant[0]), enum (s.enumError, built at compile time from s.Enum in
//     compiler.go:411-420), pattern ("does not match pattern %s" prints
//     s.Pattern), contentEncoding, contentMediaType, $ref, patternProperty.
//   - Counts and lengths derived FROM the instance but not the instance:
//     minLength / maxLength (rune length), minItems / maxItems,
//     minProperties / maxProperties, minContains / maxContains, uniqueItems
//     (indexes), additionalItems. These are a disclosure -- a secret's length
//     leaks -- but they are not the value, and shortening them would change
//     messages for every non-secret field too. Called out in the scope docs
//     instead.
//   - Names, not values: type ("expected string, but got number" -- the JSON
//     type name), required ("missing properties: a, b"), additionalProperties
//     (unevaluated property NAMES), dependencies / dependentRequired.
//   - Pure wrappers carrying no data of their own: not, allOf, anyOf, oneOf,
//     then, else, "not allowed", and the empty-message root.
//
// A library upgrade that changes any of the six format strings does not
// silently start leaking: applyKeywordRedaction fails CLOSED when the
// separator it expects is absent (see redactLeafMessage).
var valueInterpolatingKeywords = map[string]valueInterpolatingKeyword{
	"format":           {separator: " is not valid ", valueFirst: true},
	"minimum":          {separator: " but found ", valueFirst: false},
	"exclusiveMinimum": {separator: " but found ", valueFirst: false},
	"maximum":          {separator: " but found ", valueFirst: false},
	"exclusiveMaximum": {separator: " but found ", valueFirst: false},
	"multipleOf":       {separator: " not multipleOf ", valueFirst: true},
}

// redactSecretValidationError rewrites the leaf messages of a jsonschema
// validation error so no @secret field's value survives into the string
// Concept.Create returns (memql#3184).
//
// This is the concept-payload half of @secret enforcement. The concept's own
// JSON schema is the ONLY place @minimum / @maximum / @format / @minLength are
// enforced -- a mutation's args block need not mirror them -- so without this
// the annotation was bypassed entirely on this path.
//
// Three properties matter and each is load-bearing:
//
//  1. The error is a TREE (*jsonschema.ValidationError with Causes), and
//     ValidationError.Error() renders the FIRST LEAF, not the root. A nested
//     violation -- the optional-datetime oneOf puts every datetime failure one
//     level down -- carries the value on a leaf whose InstanceLocation is the
//     only thing that identifies the field. Redaction therefore walks to the
//     leaves and rewrites there.
//
//  2. Only leaves whose keyword is in valueInterpolatingKeywords AND whose
//     InstanceLocation resolves to an x-secret schema node are touched. Every
//     other message is returned byte-for-byte unchanged, which is what
//     TestConceptCreate_NonSecretMessagesAreByteIdentical pins.
//
//  3. NESTED x-secret IS SUPPORTED HERE, deliberately and explicitly.
//     Concept.SecretFields() is top-level only (and stays that way -- see
//     TestSecretFieldsIsTopLevelOnly in component/memql), so it is NOT the
//     accessor used here; instanceLocationIsSecret walks the raw definition
//     schema along the failing instance pointer instead. That is required
//     rather than optional: propertyToJSONSchema recurses for nested objects,
//     array items, map values and discriminated-union variants
//     (concept_parser.go), so @secret is expressible at any depth, and a
//     top-level-only check would silently miss it. x-secret is also treated as
//     INHERITED by everything below it, so a violation at /creds/apiKey is
//     redacted when `creds` itself is @secret.
//
// The returned error is the same *jsonschema.ValidationError with its leaf
// messages rewritten in place; the caller's %w chain and any errors.As on it
// keep working. Non-jsonschema errors are returned untouched.
func (c *Concept) redactSecretValidationError(err error) error {
	if c == nil || err == nil {
		return err
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err
	}
	root := c.definitionSchemaObject()
	if root == nil {
		return err
	}
	redactValidationErrorTree(ve, root)
	return err
}

// definitionSchemaObject decodes the concept's definition schema into a
// generic map so redaction can follow an instance pointer through it. Returns
// nil when the concept has no definition schema or it does not decode -- in
// which case redaction is skipped and messages are left as they were.
func (c *Concept) definitionSchemaObject() map[string]any {
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	return root
}

// redactValidationErrorTree walks the error tree depth-first and rewrites only
// the leaves. Interior nodes are pure wrappers ("oneOf failed", "allOf
// failed", "doesn't validate with <url>", or the empty-message root) and never
// carry an instance value, so they are recursed through rather than rewritten.
func redactValidationErrorTree(ve *jsonschema.ValidationError, root map[string]any) {
	if ve == nil {
		return
	}
	if len(ve.Causes) > 0 {
		for _, cause := range ve.Causes {
			redactValidationErrorTree(cause, root)
		}
		return
	}
	redactLeafMessage(ve, root)
}

// redactLeafMessage rewrites one leaf when, and only when, it both names a
// value-interpolating keyword and points at a secret location.
func redactLeafMessage(leaf *jsonschema.ValidationError, root map[string]any) {
	keyword := keywordFromLocation(leaf.KeywordLocation)
	spec, leaks := valueInterpolatingKeywords[keyword]
	if !leaks {
		return
	}
	if !instanceLocationIsSecret(root, leaf.InstanceLocation) {
		return
	}
	leaf.Message = applyKeywordRedaction(keyword, spec, leaf.Message)
}

// applyKeywordRedaction cuts the instance value out of one message.
//
// It fails CLOSED. If the separator the keyword's format string is built
// around is not present -- a library upgrade reworded the message, or the
// keyword name collided with a property literally named "minimum" -- the whole
// message is replaced rather than returned as-is. The alternative fails open,
// and this function only ever runs on a value already known to be secret.
func applyKeywordRedaction(keyword string, spec valueInterpolatingKeyword, message string) string {
	// LastIndex, not Index: for `format` the instance value is printed FIRST
	// and could itself contain " is not valid ", while the tail after the last
	// occurrence is jsonschema's quoted format name, which cannot.
	idx := strings.LastIndex(message, spec.separator)
	if idx < 0 {
		return fmt.Sprintf("%s constraint violated (value %s)", keyword, redactedSecretValue)
	}
	if spec.valueFirst {
		return redactedSecretValue + message[idx:]
	}
	return message[:idx+len(spec.separator)] + redactedSecretValue
}

// keywordFromLocation returns the validating keyword named by a
// ValidationError.KeywordLocation -- always its final segment
// ("/properties/rotatedAt/oneOf/0/format" -> "format").
func keywordFromLocation(location string) string {
	trimmed := strings.TrimSuffix(location, "/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}

// instanceLocationIsSecret reports whether the JSON pointer of a failing value
// resolves, through the concept's own definition schema, to a node carrying
// x-secret -- either on the node itself or on any ancestor of it.
func instanceLocationIsSecret(root map[string]any, instanceLocation string) bool {
	return schemaMarksSecret(root, parseJSONPointer(instanceLocation))
}

// schemaMarksSecret walks `tokens` down from `node`, answering true as soon as
// any node on the path is marked x-secret.
//
// Marking is INHERITED downward: reaching an x-secret node means everything
// beneath it is secret too, which is what makes a violation inside a secret
// object or array redact correctly.
//
// A path may fan out -- a token can be reachable through properties, through a
// map's additionalProperties, and through several oneOf/anyOf/allOf branches at
// once -- so every candidate is tried and any secret one wins. An
// unresolvable path answers false: an unknown location must leave the message
// untouched, which is what keeps non-secret messages byte-identical.
func schemaMarksSecret(node map[string]any, tokens []string) bool {
	if node == nil {
		return false
	}
	if secret, ok := node["x-secret"].(bool); ok && secret {
		return true
	}
	if len(tokens) == 0 {
		return false
	}
	for _, child := range childSchemas(node, tokens[0]) {
		if schemaMarksSecret(child, tokens[1:]) {
			return true
		}
	}
	return false
}

// childSchemas returns every schema node that could govern `token` beneath
// `node`, covering each shape propertyToJSONSchema emits: named properties,
// array items, map values (additionalProperties), and the branches of a
// discriminated union.
func childSchemas(node map[string]any, token string) []map[string]any {
	var out []map[string]any

	named := false
	if props, ok := node["properties"].(map[string]any); ok {
		if child, ok := props[token].(map[string]any); ok {
			out = append(out, child)
			named = true
		}
	}
	// additionalProperties governs a key only when no named property does.
	// It is also legitimately `false` in union branches, hence the assertion.
	if !named {
		if extra, ok := node["additionalProperties"].(map[string]any); ok {
			out = append(out, extra)
		}
	}
	if index, isIndex := arrayIndex(token); isIndex {
		switch items := node["items"].(type) {
		case map[string]any:
			out = append(out, items)
		case []any: // tuple form; not emitted by the concept parser, handled anyway
			if index < len(items) {
				if child, ok := items[index].(map[string]any); ok {
					out = append(out, child)
				}
			}
		}
	}
	for _, combinator := range []string{"oneOf", "anyOf", "allOf"} {
		branches, ok := node[combinator].([]any)
		if !ok {
			continue
		}
		for _, branch := range branches {
			if branchNode, ok := branch.(map[string]any); ok {
				out = append(out, childSchemas(branchNode, token)...)
			}
		}
	}
	return out
}

// arrayIndex reports whether a pointer token is an array index.
func arrayIndex(token string) (int, bool) {
	index, err := strconv.Atoi(token)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// parseJSONPointer splits an RFC 6901 pointer into its unescaped tokens. The
// empty pointer (the whole instance) yields no tokens.
func parseJSONPointer(pointer string) []string {
	if pointer == "" || pointer == "/" {
		return nil
	}
	raw := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	tokens := make([]string, 0, len(raw))
	for _, token := range raw {
		// ~1 before ~0, per RFC 6901.
		token = strings.ReplaceAll(token, "~1", "/")
		token = strings.ReplaceAll(token, "~0", "~")
		tokens = append(tokens, token)
	}
	return tokens
}
