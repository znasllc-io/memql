package procedure

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// store.go -- the engine seam, and the two actor rules this package turns on.
// They are the same two integrations/work states, for the same reason and with
// the same failure mode, and they are restated here rather than referenced
// because both fail SILENTLY.
//
// RULE 1: EVERY WRITE NEEDS INTERNAL ORIGIN. The authoring mutations this
// package calls are @serverOnly, and auth.OriginFromContext defaults to
// OriginClient. A @serverOnly construct on any other origin is refused with
// ONE WARN and the call carries on: the row is never inserted and nothing
// above hears about it. executeInternal is the ONE stamp site, the marked
// context is a LOCAL, and it is never returned -- a returned one would be
// inherited by a later frame and would open every other @serverOnly construct
// in the tree for the rest of the call.
//
// RULE 2: EVERY READ NEEDS THE OWNER'S ACTOR. Every work concept declares
// @rowAuthz(owner="ownerUserId", clusterOwner) and the READ gate has no
// internal-origin bypass: an unstamped read returns ZERO ROWS AND NO ERROR,
// which reads here as "this owner has no recordings" -- indistinguishable from
// a corpus that genuinely has none, and it would make every sweep quietly
// learn nothing.
//
// The composite tier has a second consequence this package cares about more
// than work does: mining runs the corpus of ONE owner. A cluster-owner read
// would blend two people's recordings into one template, and the procedure
// that came out would be correct about nobody.
//
// TWO READS ARE THE EXCEPTION, and both are made by the cluster's maintenance
// principal BEFORE any owner is known (gap G10): usersForSeedSweep, to list the
// owners a sweep walks, and workApprovalById, to learn whose construct a
// decided promotion names. Both are @serverOnly, so they go through
// executeInternal, and neither is reachable by anybody else: workApprovalById
// filters to a cluster owner, which a person and an ordinary automation's
// reader are not, and each handler admits only the cluster's principal before
// it reads either. Every read after them borrows the owner. (The completion
// trigger that fires the lift needs no such read: its event names the run's
// owner, which the learn handler borrows and re-verifies.)

// Engine is the executor seam.
type Engine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

type store struct {
	engine Engine
}

// ownerActor stamps the row owner's actor. A blank owner returns ctx
// unchanged, as auth.ContextWithUserActor would: callers that need a non-empty
// owner check for one themselves and say what they were doing.
func ownerActor(ctx context.Context, ownerUserId string) context.Context {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return ctx
	}
	return auth.ContextWithUserActor(ctx, owner)
}

// executeInternal runs one @serverOnly construct. THE ONE STAMP SITE in this
// package; it returns a RESULT, never a context.
func (s *store) executeInternal(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("procedure: engine not configured")
	}
	res, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstConstruct(query), err)
	}
	return memqlRows(res), nil
}

// writeInternal runs one @serverOnly mutation. A wrapper rather than a second
// stamp, which is what keeps the call-site count at one.
func (s *store) writeInternal(ctx context.Context, query string) error {
	_, err := s.executeInternal(ctx, query)
	return err
}

// query runs a read UNSTAMPED, so the actor in ctx decides the scope.
// Stamping here would widen the read silently.
func (s *store) query(ctx context.Context, q string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("procedure: engine not configured")
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstConstruct(q), err)
	}
	return memqlRows(res), nil
}

// The @serverOnly READS the header names live HERE, beside the one stamp,
// rather than at their callers: a @serverOnly call belongs next to the
// internal origin it needs, where the next reader -- and the conformance gate
// that holds every such call to a file that stamps -- finds the two together.

// activeUserIds lists every active person, through the query the seed sweep
// uses -- unscoped by nature, because a sweep over owners cannot know whose
// corpus to read before it has the list. Its caller admits only the cluster's
// principal first.
func (s *store) activeUserIds(ctx context.Context) ([]map[string]any, error) {
	return s.executeInternal(ctx, "query usersForSeedSweep()")
}

// approvalByIdAsCluster reads one approval through the by-id read, which only a
// cluster owner's actor answers: the promotion
// decision's automation learning whose construct a DECIDED approval names. A
// person has no by-id read of a decided approval (their list is the pending
// one), and the caller admits only the cluster's principal first. nil when
// nothing answered.
func (s *store) approvalByIdAsCluster(ctx context.Context, approvalId string) (map[string]any, error) {
	rows, err := s.executeInternal(ctx, "query "+call("workApprovalById", map[string]any{"approvalId": approvalId}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// --- call-string construction --------------------------------------------

// call renders a construct call. STRINGS GO THROUGH langparser.QuoteString,
// never Go's %q: the two escape sets differ, and a value Go quotes one way and
// the MemQL lexer reads another is a call that parses into something the
// caller did not write.
func call(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	first := true
	for _, k := range keys {
		v := args[k]
		// A nil argument is DROPPED, never rendered as `nil`: "the caller said
		// nothing" and "the caller said nil" must not render the same way. The
		// TYPED nils matter as much as the untyped one -- a nil map inside an
		// `any` is not == nil, and rendered it would be a value.
		if isNilValue(v) {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(literal(v))
	}
	b.WriteByte(')')
	return b.String()
}

// literal renders one value as the MemQL literal the engine's call grammar
// reads back as that value (gap G9, epic memql#5408).
//
// MAPS AND LISTS ARE LITERALS TOO, recursively. The first renderer here fell
// through to a quoted string for anything it did not name, so the Gate 1
// report went in as the TEXT "map[gate1Ran:true ...]" -- a string where the
// concept declares an object, refused by the schema the moment the row was
// validated. The payload this package now writes is an object of objects.
//
// A map's keys are written in sorted order, BARE when the lexer reads the key
// back as one plain identifier and QUOTED otherwise: a hole id like
// `s0.command.3`, a tool name like `docker-compose` or a keyword like `if` is
// a legitimate key, and the call grammar accepts a quoted key where a bare one
// would lex as something else. A nil inside a map is dropped for call's
// reason; a nil inside a list is kept as `nil`, because dropping it would
// shift every later element.
func literal(v any) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case string:
		return langparser.QuoteString(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return numberLiteral(t)
	case json.Number:
		return t.String()
	case []string:
		parts := make([]string, len(t))
		for i, s := range t {
			parts[i] = langparser.QuoteString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = literal(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []map[string]any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = literal(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return objectLiteral(t)
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		return objectLiteral(m)
	default:
		return langparser.QuoteString(fmt.Sprint(t))
	}
}

// objectLiteral renders a map as `{k: v, ...}`, keys sorted.
func objectLiteral(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if isNilValue(v) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, objectKey(k)+": "+literal(m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// objectKey writes a map key bare exactly when the lexer reads it back as the
// same single identifier -- so the rule is the lexer's own, and a keyword the
// grammar adds later is quoted rather than broken -- and quoted otherwise.
func objectKey(k string) string {
	if plainName.MatchString(k) {
		toks, err := langparser.NewLexer(k).Tokenize()
		if err == nil && len(toks) >= 1 && toks[0].Type == langparser.TokenIdentifier && toks[0].Literal == k &&
			(len(toks) == 1 || toks[1].Type == langparser.TokenEOF) {
			return k
		}
	}
	return langparser.QuoteString(k)
}

// plainName is the shape a bare key must have before the lexer is asked.
var plainName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// numberLiteral writes a decoded JSON number the way the call grammar reads a
// number: integral values without a fraction or an exponent, everything else
// in its shortest exact form. A value no JSON document can hold (NaN, an
// infinity) has no literal, and `nil` is the honest spelling of "no number".
func numberLiteral(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "nil"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// isNilValue reports an untyped nil or a typed nil map or slice -- the values
// that mean "said nothing" rather than "said empty". An EMPTY map is not one:
// `{}` is a value, and it is how recordConstructLadder is told to clear a
// streak's bindings.
func isNilValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		return t == nil
	case map[string]string:
		return t == nil
	case []any:
		return t == nil
	case []string:
		return t == nil
	case []map[string]any:
		return t == nil
	}
	return false
}

func firstConstruct(q string) string {
	fields := strings.Fields(q)
	if len(fields) >= 2 {
		name := fields[1]
		if i := strings.IndexByte(name, '('); i > 0 {
			return name[:i]
		}
		return name
	}
	return "procedure"
}

// memqlRows lowers an execute result into rows. A query answers with a
// GraphBundle; a builtin answers with one id-keyed map. Both shapes arrive
// here, and flattening them in one place is what keeps every caller from
// having to know which construct kind it just ran.
func memqlRows(res *memqlengine.ExecuteResult) []map[string]any {
	if res == nil {
		return nil
	}
	raw := res.OutputPayload()
	if bundle, ok := raw.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{"id": n.GetId(), "concept": n.GetConcept()}
			if payload := n.GetPayload(); payload != nil {
				maps.Copy(row, payload.AsMap())
			}
			out = append(out, row)
		}
		return out
	}
	return rowsFromAny(raw)
}

func rowsFromAny(raw any) []map[string]any {
	switch t := raw.(type) {
	case nil:
		return nil
	case []map[string]any:
		return t
	case map[string]any:
		// An id-keyed map of rows, or one row. A value that is itself a map
		// with an id means the outer map is the index.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]map[string]any, 0, len(t))
		for _, k := range keys {
			if inner, ok := t[k].(map[string]any); ok {
				out = append(out, inner)
			}
		}
		if len(out) == len(t) && len(t) > 0 {
			return out
		}
		return []map[string]any{t}
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func str(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func obj(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if o, ok := v.(map[string]any); ok {
			return o
		}
	}
	return nil
}
