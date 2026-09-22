package procedure

import (
	"context"
	"fmt"
	"maps"
	"sort"
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
		// nothing" and "the caller said nil" must not render the same way.
		if v == nil {
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

func literal(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprint(t)
	case int64:
		return fmt.Sprint(t)
	case float64:
		return fmt.Sprint(t)
	case []string:
		parts := make([]string, len(t))
		for i, s := range t {
			parts[i] = langparser.QuoteString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return langparser.QuoteString(fmt.Sprint(t))
	}
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
