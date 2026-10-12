package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/librarykey"
)

// store.go -- the engine seam, and the two actor rules the Materializer
// turns on. They are the work spine's rules and they are restated here
// rather than shared, because both fail SILENTLY when they are missing
// and a reader of this package must be able to find them without
// reading another one.
//
// ===========================================================================
// RULE 1: EVERY COMPOSITION LIFECYCLE WRITE NEEDS INTERNAL ORIGIN
// ===========================================================================
// dsl/compose/mutations.memql declares createComposition, createCompositionInput,
// updateCompositionState and recordComposeRecipeRun @serverOnly.
// auth.OriginFromContext defaults to OriginClient (the zero value) and the
// function validator refuses a @serverOnly construct on any other origin
// with ONE WARN and carries on. For a write that means the row is never
// inserted and nothing above it hears about it: a composition that
// reports an id nothing stored, a `ready` status the app never sees, a
// recipe whose run count never moves.
//
// So executeInternal below stamps auth.ContextWithInternalOrigin INLINE,
// as the argument to the one Execute that needs it -- the ONE stamp site
// in this package, which TestTheStampNeverEscapesItsCall counts. The
// marked context is a LOCAL and is never returned: a returned one is
// inherited by every later frame and opens every OTHER @serverOnly
// construct in the tree for the rest of the call (memql#2879 /
// memql#2989).
//
// ===========================================================================
// RULE 2: EVERY READ AND EVERY OWNED WRITE NEEDS AN ACTOR THAT IS THE OWNER
// ===========================================================================
// Every compose concept declares @rowAuthz(owner="ownerUserId"). Public
// records also grant cluster-owner and account reads; input snapshots do not.
//
//   - The READ gate has NO internal-origin bypass and answers "no
//     identity, no rows". An unstamped read returns ZERO ROWS AND NO
//     ERROR -- which reads as "you have materialized nothing", a
//     completely plausible answer and therefore the dangerous one.
//   - The WRITE guard ignores the second argument (memql#4312), so a
//     cluster owner does not get to rewrite somebody else's row. A write
//     onto a composition belonging to a person must run AS that person.
//
// The MATERIALIZE PATH RUNS UNDER THE CALLER THROUGHOUT -- it never
// borrows anybody's authority, because everything it touches is the
// caller's own: their sources, their template, their Library folder.
// That is a stronger position than the campaigns drain worker's and it
// is worth keeping: it means a template a caller cannot read is refused
// rather than rendered through, and a source they cannot read simply
// does not come back.

// Engine is the narrow surface this package needs. *memql.MemQLEngine
// satisfies it directly.
type Engine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

type store struct {
	engine Engine
}

// executeInternal runs one @serverOnly construct under a context this
// function constructs. THE ONE STAMP SITE -- see the header.
func (s *store) executeInternal(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("compose: engine not configured")
	}
	res, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstConstruct(query), err)
	}
	return memqlRows(res), nil
}

// writeInternal runs one @serverOnly mutation. A wrapper rather than a
// second stamp, which keeps the call-site count at one.
func (s *store) writeInternal(ctx context.Context, query string) error {
	_, err := s.executeInternal(ctx, query)
	return err
}

// query runs a read UNSTAMPED, so the actor in ctx decides the scope.
// Stamping here would widen it silently and move the decision away from
// the actor, which is the failure nobody notices.
func (s *store) query(ctx context.Context, q string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("compose: engine not configured")
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstConstruct(q), err)
	}
	return memqlRows(res), nil
}

// write runs an ordinary owned mutation under the caller's own actor.
func (s *store) write(ctx context.Context, q string) error {
	_, err := s.query(ctx, q)
	return err
}

// ---------------------------------------------------------------------------
// Actors
// ---------------------------------------------------------------------------

// requirePrincipal admits a signed-in person and refuses everything else.
//
// The floor is the HANDLER's, because a builtin's annotation set carries
// no @requiresRank. It is deliberately NOT a rank floor: these
// capabilities are caller-scoped, and the row tier decides which
// compositions are the caller's. A connector or an anonymous actor is
// refused BY NAME so the reason is legible in a log line.
func requirePrincipal(ctx context.Context) (*auth.AccessContext, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return nil, fmt.Errorf("compose: no authenticated caller")
	}
	if ac.IsAnonymousActor() || ac.IsConnector() || strings.TrimSpace(ac.UserId) == "" {
		return nil, fmt.Errorf("compose: an anonymous or connector actor may not materialize")
	}
	return ac, nil
}

// ---------------------------------------------------------------------------
// Reads (caller-scoped: run UNSTAMPED under whatever actor ctx carries)
// ---------------------------------------------------------------------------

func (s *store) compositionById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("compositionById", map[string]any{"compositionId": id})))
}

func (s *store) compositionExecutionById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("compositionExecutionById", map[string]any{"compositionId": id})))
}

func (s *store) compositionInputById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("compositionInputById", map[string]any{"compositionId": id})))
}

func (s *store) templateById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("composeTemplateById", map[string]any{"templateId": id})))
}

func (s *store) recipeById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("composeRecipeById", map[string]any{"recipeId": id})))
}

func (s *store) libraryFileById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("libraryFileById", map[string]any{"fileId": id})))
}

// libraryFolderById is the caller's folder, and nil when it is not theirs or was
// deleted. An archived folder IS answered (its row says archived): the Library
// keeps naming a folder somebody put away, so the caller reads that off the row.
func (s *store) libraryFolderById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("libraryFolderById", map[string]any{"folderId": id})))
}

// emailLayoutById is an organization email layout, by its bare id, and nil when
// the caller cannot read it: the organization boundary answers a layout of an
// organization they are not in exactly as it answers one that does not exist.
// An archived layout IS answered (its row says so), as the Email app keeps it
// readable for the emails made from it.
func (s *store) emailLayoutById(ctx context.Context, id string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("emailLayoutById", map[string]any{"layoutId": id})))
}

// libraryArtifactForFile is the caller's own Library artifact for a file, and
// nil when they have none: the query is held to the artifact's owner, so even a
// file they can read has no artifact here unless it is theirs.
func (s *store) libraryArtifactForFile(ctx context.Context, fileId string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("libraryArtifactForFile", map[string]any{"fileId": fileId})))
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

func (s *store) createComposition(ctx context.Context, args map[string]any) error {
	return s.writeInternal(ctx, "mutation "+call("createComposition", args))
}

func (s *store) createCompositionInput(ctx context.Context, args map[string]any) error {
	return s.writeInternal(ctx, "mutation "+call("createCompositionInput", args))
}

func (s *store) updateCompositionState(ctx context.Context, args map[string]any) error {
	return s.writeInternal(ctx, "mutation "+call("updateCompositionState", args))
}

func (s *store) recordRecipeRun(ctx context.Context, args map[string]any) error {
	return s.writeInternal(ctx, "mutation "+call("recordComposeRecipeRun", args))
}

// createLibraryFile files the output. @serverOnly, because blobUrl is a
// storage path the node's credential reads back: a client that could author it
// could point a row it owns at somebody else's object. So it runs through the
// one stamp site -- after the key is held to the CALLER's own
// library/{owner}/ prefix, which is what makes stamping this request-derived
// write safe. storeBytes composes the key from the caller's own id; the check
// is what keeps that true for anything that reaches here later.
func (s *store) createLibraryFile(ctx context.Context, args map[string]any) error {
	owner := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		owner = ac.UserId
	}
	blobUrl, _ := args["blobUrl"].(string)
	if err := librarykey.Check(blobUrl, owner, ""); err != nil {
		return fmt.Errorf("compose: refusing to file an output whose key is not the caller's own: %w", err)
	}
	return s.writeInternal(ctx, "mutation "+call("createLibraryFile", args))
}

func (s *store) setLibraryFileReady(ctx context.Context, fileId, summary string) error {
	return s.write(ctx, "mutation "+call("setLibraryFileStatus", map[string]any{
		"fileId": fileId, "status": "ready", "summary": summary,
	}))
}

// ---------------------------------------------------------------------------
// Call composition
// ---------------------------------------------------------------------------

// call composes `name(k: v, ...)` with bare-identifier keys and
// JSON-encoded values, keys sorted so a log line is stable.
//
// STRINGS GO THROUGH langparser.QuoteString, NEVER Go's %q. The two
// escape grammars diverge on four control bytes -- %q emits \x00, \a, \v
// and \xNN, and the MemQL lexer's readString implements the JSON escapes
// and rejects every one of them, so one such byte in an interpolated
// value makes the whole statement unparseable (memql#3035, memql#3611).
// A composition's name and statement are a person's free text and a
// source label is a row's own field, so this is not a theoretical
// difference here -- it is the likeliest single cause of a materialize
// that fails with a parse error naming nothing.
func call(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	first := true
	for _, k := range keys {
		v := args[k]
		// A nil argument is DROPPED, never rendered as `null`. An
		// optional field given null fails the concept's type check and
		// the engine refuses the whole row, so "the caller said nothing"
		// and "the caller said null" must not render the same way. The
		// typed-nil arms matter as much as the untyped one: a nil map
		// inside an `any` is not == nil.
		if v == nil || isNilValue(v) {
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

func isNilValue(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return t == nil
	case []map[string]any:
		return t == nil
	case []string:
		return t == nil
	case []any:
		return t == nil
	}
	return false
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
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return "null"
	}
	return string(b)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// firstConstruct names the construct in an error, so a failure says
// which call failed rather than only that one did.
func firstConstruct(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, " ("); i > 0 {
		head := q[:i]
		rest := strings.TrimSpace(q[i:])
		if j := strings.IndexByte(rest, '('); j > 0 {
			return head + " " + strings.TrimSpace(rest[:j])
		}
		return head
	}
	return "compose"
}

// ---------------------------------------------------------------------------
// Row decoding
// ---------------------------------------------------------------------------

// memqlRows normalises whatever the engine handed back into plain rows.
//
// BOTH BRANCHES ARE REACHABLE AND BOTH ARE NEEDED. Every compose query
// carries a shape, and a SHAPED query returns `output` rows and NILS the
// GraphBundle (memql, the adding-a-shape-drops-the-bundle rule) -- so a
// caller that only handled the bundle branch reads an empty result from
// a perfectly good query. A mutation answers with a bundle.
func memqlRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	if res, ok := raw.(*memqlengine.ExecuteResult); ok {
		if res == nil {
			return nil
		}
		raw = res.OutputPayload()
	}
	if raw == nil {
		return nil
	}
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
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

func one(rows []map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}
