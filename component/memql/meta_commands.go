// Meta-command dispatch for introspection builtins (help / docs /
// concepts / validate / functions / tools / serviceVersion /
// memqlVersion / shapeTemplates / shapeHelp / contentId /
// previewInsert). These calls are NOT real queries against the data
// layer -- they are admin / introspection meta-commands that produce
// DSL-shaped output.
//
// Until #256 the in-package memql parser owned the dispatch surface
// (parser.go's lookupBuiltin -> parseBuiltinFunctionCall path). The
// langparser had no equivalent, so the #249 default-flip surfaced
// these as generic *FunctionCallExpr and the engine had no expansion
// path. The shim here lives ABOVE both parsers -- e.Parse short-
// circuits to tryParseMetaCommand BEFORE either tokeniser fires, so
// the runtime parsers no longer own introspection logic.
//
// The shim consults the existing FunctionRegistry for the
// authoritative list of builtin names + their argument contracts;
// there is no separate registry duplicating that surface. Argument
// parsing is dedicated to this file and uses only encoding/json +
// a small bare-identifier-key quoter, so neither runtime parser is
// invoked.
package memql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// tryParseMetaCommand attempts to short-circuit query as an
// introspection builtin call. The shim is the runtime-side analogue
// of e.Parse's isInsertFunction short-circuit: when the whole query
// is a `<knownBuiltin>(<args>)` invocation, return the corresponding
// *BuiltinFunctionExpression so e.Parse can wrap it in a QueryPlan
// without touching either tokeniser.
//
// Returns:
//   - (expr, true, nil)  -- matched a registered builtin; args parsed.
//   - (nil, true, err)   -- matched a registered builtin name but the
//     arguments failed validation; this is a real parse error and the
//     caller MUST propagate it (do NOT fall through to a parser).
//   - (nil, false, nil)  -- not a meta-command shape; the regular
//     parser dispatch should run.
func (e *MemQLEngine) tryParseMetaCommand(query string) (*BuiltinFunctionExpression, bool, error) {
	if e == nil {
		return nil, false, nil
	}
	name, argsSrc, ok := splitMetaCommandCall(query)
	if !ok {
		return nil, false, nil
	}
	fn, ok := e.lookupBuiltinFunction(name)
	if !ok {
		return nil, false, nil
	}
	args, err := parseMetaCommandArgs(name, argsSrc, fn.BuiltinArgs)
	if err != nil {
		return nil, true, err
	}
	resultName := name
	if strings.EqualFold(fn.Executor, BuiltinExecutorServiceVersion) {
		// Preserve historical compatibility: both memqlVersion() and
		// serviceVersion() normalize to memqlVersion in the parsed
		// expression. Mirrors parser.go's parseBuiltinFunctionCall.
		resultName = "memqlVersion"
	}
	return &BuiltinFunctionExpression{
		Name:     resultName,
		Executor: fn.Executor,
		Args:     args,
	}, true, nil
}

// lookupBuiltinFunction resolves a name against the engine's Function
// registry, returning the Function only when it is a builtin
// (FunctionTypeBuiltin). The existing FunctionRegistry IS the
// meta-command catalogue -- there is no second source of truth.
// Resolution is exact on the primary name (the registry's own lookup
// semantics), then case-insensitive over the builtins' declared @alias
// names (BuiltinAliases) on a miss, so memqlVersion() keeps resolving
// to the serviceVersion builtin now that the expression-builtin
// special-case is retired (#2707) -- the alias lives in the registry,
// not in either parser. The alias scan uses the non-cloning Range walk
// (sorted-name order, so the first claimant is deterministic); only
// the matched entry is cloned via a second Get.
func (e *MemQLEngine) lookupBuiltinFunction(name string) (*Function, bool) {
	if e == nil || e.functions == nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(name)
	fn, err := e.functions.Get(trimmed)
	if err != nil || fn == nil {
		primary := ""
		e.functions.Range(func(_ string, cand *Function) bool {
			if cand == nil || !cand.IsBuiltin() {
				return true
			}
			for _, alias := range cand.BuiltinAliases {
				if strings.EqualFold(alias, trimmed) {
					primary = cand.Name
					return false
				}
			}
			return true
		})
		if primary == "" {
			return nil, false
		}
		fn, err = e.functions.Get(primary)
		if err != nil || fn == nil {
			return nil, false
		}
	}
	if !fn.IsBuiltin() {
		return nil, false
	}
	// A @disabled builtin does not match (#2608 review): the bare call then
	// flows through normal parsing into the expansion gate, producing the
	// same "function %q is disabled" rejection as the nested form -- one
	// gate, both invocation shapes.
	if !fn.Enabled {
		return nil, false
	}
	return fn, true
}

// splitMetaCommandCall recognises `<name>(<args>)` where the whole
// trimmed input is consumed by the call and returns the name +
// the raw args substring. Quoted strings and nested parens inside
// args are skipped so a closing paren inside a string literal or
// nested call doesn't end the match prematurely.
//
// Returns (_, _, false) on any input that isn't a clean
// single-call invocation -- including comparisons, prefixes /
// suffixes around the call, or trailing tokens after the close
// paren.
func splitMetaCommandCall(query string) (name, argsSrc string, ok bool) {
	runes := []rune(query)
	n := len(runes)
	i := 0
	for i < n && isMetaWhitespace(runes[i]) {
		i++
	}
	if i == n || !isMetaIdentStart(runes[i]) {
		return "", "", false
	}
	nameStart := i
	for i < n && isMetaIdentChar(runes[i]) {
		i++
	}
	name = string(runes[nameStart:i])
	for i < n && isMetaWhitespace(runes[i]) {
		i++
	}
	if i == n || runes[i] != '(' {
		return "", "", false
	}
	i++ // consume '('
	argsStart := i
	depth := 1
	inStr := false
	var quote rune
	for i < n {
		c := runes[i]
		if inStr {
			if c == '\\' && i+1 < n {
				i += 2
				continue
			}
			if c == quote {
				inStr = false
			}
			i++
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = true
			quote = c
			i++
		case '(':
			depth++
			i++
		case ')':
			depth--
			if depth == 0 {
				argsSrc = string(runes[argsStart:i])
				i++
				for i < n && isMetaWhitespace(runes[i]) {
					i++
				}
				if i != n {
					return "", "", false
				}
				return name, argsSrc, true
			}
			i++
		default:
			i++
		}
	}
	return "", "", false
}

// parseMetaCommandArgs decodes argsSrc into the args map expected by
// the builtin executor handlers (executor_builtin.go) using only
// encoding/json + a permissive bare-identifier-key pre-processor. The
// arg shapes match the existing BuiltinArgContract surface so the
// downstream contract validator (validateBuiltinCallArgs) catches
// missing required fields and unexpected properties the same way
// parser.go's parseBuiltinFunctionCall does.
func parseMetaCommandArgs(name, argsSrc string, contract *BuiltinArgContract) (map[string]any, error) {
	if contract == nil {
		contract = &BuiltinArgContract{Profile: BuiltinArgProfileNone}
	}
	profile := contract.Profile
	if profile == "" {
		profile = BuiltinArgProfileNone
	}
	trimmed := strings.TrimSpace(argsSrc)

	var (
		args map[string]any
		err  error
	)
	switch profile {
	case BuiltinArgProfileNone:
		if trimmed != "" {
			return nil, fmt.Errorf("%s() does not accept arguments", name)
		}
	case BuiltinArgProfileObject:
		body, ok := objectArgBody(trimmed)
		if !ok {
			return nil, fmt.Errorf("%w: %s() requires a JSON object argument", ErrInvalidArgument, name)
		}
		args, err = decodeMetaObjectArg(body)
		if err != nil {
			return nil, fmt.Errorf("%w: %s() argument: %v", ErrInvalidArgument, name, err)
		}
	case BuiltinArgProfileOptionalObject:
		if trimmed != "" {
			body, ok := objectArgBody(trimmed)
			if !ok {
				return nil, fmt.Errorf("%w: %s() argument must be an object", ErrInvalidArgument, name)
			}
			args, err = decodeMetaObjectArg(body)
			if err != nil {
				return nil, fmt.Errorf("%w: %s() argument: %v", ErrInvalidArgument, name, err)
			}
		}
	case BuiltinArgProfileStringOrObject:
		if trimmed == "" {
			return nil, fmt.Errorf("%w: %s() requires a string or object argument", ErrInvalidArgument, name)
		}
		args, err = decodeMetaStringOrObjectArg(name, trimmed, contract)
		if err != nil {
			return nil, err
		}
	case BuiltinArgProfileOptionalString:
		if trimmed != "" {
			s, derr := decodeMetaStringArg(trimmed)
			if derr != nil {
				return nil, fmt.Errorf("%w: %s() argument must be a string", ErrInvalidArgument, name)
			}
			args = map[string]any{stringKeyOrDefault(contract.StringKey): s}
		}
	case BuiltinArgProfileOptionalStringOrObject:
		if trimmed != "" {
			args, err = decodeMetaStringOrObjectArg(name, trimmed, contract)
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unsupported builtin argument profile %q for %s()", profile, name)
	}

	if err := validateBuiltinCallArgs(name, args, contract); err != nil {
		return nil, err
	}
	if len(args) == 0 {
		args = nil
	}
	return args, nil
}

func decodeMetaStringOrObjectArg(name, trimmed string, contract *BuiltinArgContract) (map[string]any, error) {
	if trimmed[0] == '{' {
		args, err := decodeMetaObjectArg(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%w: %s() argument: %v", ErrInvalidArgument, name, err)
		}
		return args, nil
	}
	s, err := decodeMetaStringArg(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %s() argument must be a string or object", ErrInvalidArgument, name)
	}
	return map[string]any{stringKeyOrDefault(contract.StringKey): s}, nil
}

func stringKeyOrDefault(key string) string {
	if key == "" {
		return "value"
	}
	return key
}

// decodeMetaObjectArg parses a {...} object literal where keys may be
// either bare identifiers (parser.go's canonical form) or quoted
// strings (JSON-serialised tool calls). Falls through to standard
// encoding/json after the bare-identifier quoter pre-processes the
// source. Number values decode to float64; that matches what
// parser.go's parseFunctionArgValue emits for tokNumber, so #255 is
// the right place to standardise on int when both parsers converge.
// namedArgsBody matches the leading key of the named-args call form, in BOTH
// spellings the renderer produces: a bare identifier and a quoted one.
//
// The quoted alternative requires the CLOSING quote before the colon, which is
// what keeps a plain string argument out. `"v1:x"` looks like a quoted key
// followed by a colon to any looser pattern, and admitting it would turn a
// clean "requires a JSON object argument" into a JSON decoder error about a
// body this never should have built.
//
// Anchored, and it deliberately does not validate the rest --
// decodeMetaObjectArg is what decides whether the body is really an object, so
// a near-miss still fails with the decoder's own message rather than with a
// guess from a regex.
var namedArgsBody = regexp.MustCompile(`^(?:"[A-Za-z_][A-Za-z0-9_]*"|[A-Za-z_][A-Za-z0-9_]*)\s*:`)

// objectArgBody normalises what an `object`-profile builtin was called with
// into a JSON object body, and answers false when there is nothing there to
// read as one.
//
// IT ACCEPTS TWO SPELLINGS BECAUSE THE LANGUAGE ONLY EMITS ONE OF THEM
// (memql#4927). The profile was written against `name({a: 1})` and checked
// `trimmed[0] == '{'`; memql#2335 then RETIRED that wrapper -- the rewriter
// lowers every call to `name(a: 1)` and "the parser now rejects the wrapper".
// So an `object` builtin could only be called in a spelling the DSL refuses,
// and every automation step reaching one was refused at parse:
//
//	packageNoteUpstreamFromWebhook() requires a JSON object argument
//
// with the args plainly there in the message. Both automations that pass
// named arguments to a builtin were dead in exactly this way -- the inbound
// webhook that notes a package's upstream moving, and campaign bounce and
// complaint ingestion -- and both fail only when the automation FIRES, which
// is why a green suite said nothing. It is the same disagreement memql#4927
// reported from its other side, where an empty argument list met the same
// check.
//
// The braced form stays accepted: it is what an HTTP or SDK caller sends, and
// it is still what `previewInsert({...})` and its neighbours are written as.
//
// Both key spellings are accepted because the step renderer emits the QUOTED
// one (`renderFunctionArgs` strips the braces off renderMemQLValue's object)
// while a hand-written call reads bare. Matching only the bare form would fix
// the shape nobody sends.
func objectArgBody(trimmed string) (string, bool) {
	if trimmed == "" {
		return "", false
	}
	if trimmed[0] == '{' {
		return trimmed, true
	}
	if namedArgsBody.MatchString(trimmed) {
		return "{" + trimmed + "}", true
	}
	return "", false
}

func decodeMetaObjectArg(src string) (map[string]any, error) {
	quoted, err := quoteBareIdentifierKeys(src)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(quoted), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// decodeMetaStringArg unquotes a `"..."` string literal using JSON's
// escape rules. The shim accepts the canonical double-quoted form
// only -- the in-package parser tolerates single quotes for some
// tokens, but introspection-builtin tests + every caller in the
// repo use double quotes.
func decodeMetaStringArg(src string) (string, error) {
	var out string
	if err := json.Unmarshal([]byte(src), &out); err != nil {
		return "", err
	}
	return out, nil
}

// quoteBareIdentifierKeys walks src and wraps every bare-identifier
// object key in double quotes so the result is valid JSON. Keys are
// identifiers (start with letter / underscore, then letters / digits /
// underscores) that appear in key position: immediately after `{` or
// `,` and followed by `:` (modulo whitespace). Quoted keys and string
// literals are left untouched so a value like `{"label": "foo:bar"}`
// is not mistaken for a key.
func quoteBareIdentifierKeys(src string) (string, error) {
	runes := []rune(src)
	n := len(runes)
	var buf strings.Builder
	buf.Grow(n + 16)

	inStr := false
	var quote rune
	atKeyPosition := false

	for i := 0; i < n; i++ {
		c := runes[i]
		if inStr {
			buf.WriteRune(c)
			if c == '\\' && i+1 < n {
				buf.WriteRune(runes[i+1])
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			quote = c
			buf.WriteRune(c)
			atKeyPosition = false
			continue
		}
		if isMetaWhitespace(c) {
			buf.WriteRune(c)
			continue
		}
		if c == '{' || c == ',' {
			buf.WriteRune(c)
			atKeyPosition = true
			continue
		}
		if atKeyPosition && isMetaIdentStart(c) {
			// Walk the identifier
			start := i
			for i < n && isMetaIdentChar(runes[i]) {
				i++
			}
			ident := string(runes[start:i])
			// Peek past whitespace; if next non-ws is ':', wrap the
			// identifier in quotes.
			j := i
			for j < n && isMetaWhitespace(runes[j]) {
				j++
			}
			if j < n && runes[j] == ':' {
				buf.WriteRune('"')
				buf.WriteString(ident)
				buf.WriteRune('"')
				i-- // re-position so the outer loop continues from i (the next iteration's i++ takes us forward by one)
				atKeyPosition = false
				continue
			}
			// Not a key -- write the identifier as-is. encoding/json
			// will reject this; surface the failure with the rest of
			// the unparseable substring intact so callers can see what
			// went wrong.
			buf.WriteString(ident)
			i--
			atKeyPosition = false
			continue
		}
		buf.WriteRune(c)
		atKeyPosition = false
	}
	return buf.String(), nil
}

func isMetaWhitespace(c rune) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isMetaIdentStart(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isMetaIdentChar(c rune) bool {
	return isMetaIdentStart(c) || (c >= '0' && c <= '9')
}
