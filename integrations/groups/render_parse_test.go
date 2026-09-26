package groups

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// render_parse_test.go -- every statement this package composes, run through
// the REAL MemQL front end (no database).
//
// The second level, for the reason integrations/release records: a handler
// covered only against an engine that RECORDS call strings can ship a call the
// parser has never accepted, fail at parse on every real cluster, and stay
// green forever. Five guest mutations and the whole deploy-control surface
// each shipped that way (memql#4256, memql#4258).
//
// Two different things are checked, and the second is not implied by the
// first: that the string PARSES and its function RESOLVES against the embedded
// dsl/ tree, and that every argument it passes is DECLARED -- because an
// undeclared argument is silently DROPPED rather than refused, which is how
// revokeAuthSession was called with seven arguments against a two-argument
// declaration for its whole life.

func realEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (the dsl/ tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil {
		t.Fatal("no concept registry")
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

// recordingEngine captures every statement the store composes, so the tests
// below run the REAL rendering code rather than a copy of it.
type recordingEngine struct{ calls []string }

func (r *recordingEngine) Execute(_ context.Context, query string) (any, error) {
	r.calls = append(r.calls, query)
	return map[string]any{"rows": []any{}}, nil
}

// productionStatements drives every write this package makes and returns the
// statements it composed.
//
// The values are AWKWARD on purpose. langparser.QuoteString and Go's %q agree
// on quotes, backslashes, newlines, tabs and non-ASCII -- so a fixture built
// from the values you would naturally think of passes under both, and a
// renderer refactored to %q looks fine. They diverge on exactly four bytes:
// NUL, BEL, VT and DEL, which %q renders as \x00 / \a / \v / \x7f and the
// MemQL lexer rejects with "invalid escape character". A group named by a
// person really can carry any of them.
func productionStatements(t *testing.T) []string {
	t.Helper()
	awkward := "Acme \"Group\" \\ and\nmore\x00 \a \v \x7f"
	rec := &recordingEngine{}
	s := NewStore(rec)
	ctx := context.Background()

	if err := s.WriteGroup(ctx, Group{
		ID: "g-1", Name: awkward, Description: awkward, Kind: KindCustom,
		AccountID: "acct-1", Status: StatusActive,
	}); err != nil {
		t.Fatalf("WriteGroup(active): %v", err)
	}
	if err := s.WriteGroup(ctx, Group{
		ID: "g-1", Name: "x", Kind: KindAccount, AccountID: "acct-1", Status: StatusArchived,
	}); err != nil {
		t.Fatalf("WriteGroup(archived): %v", err)
	}
	if err := s.WriteMembership(ctx, Membership{
		ID: "g-1-u-1", GroupID: "g-1", UserID: "u-1", Origin: OriginAdded, Status: StatusActive,
	}, "u-admin", ""); err != nil {
		t.Fatalf("WriteMembership(active): %v", err)
	}
	if err := s.WriteMembership(ctx, Membership{
		ID: "g-1-u-1", GroupID: "g-1", UserID: "u-1", Origin: OriginDomain, Status: StatusRemoved,
	}, "", "u-admin"); err != nil {
		t.Fatalf("WriteMembership(removed): %v", err)
	}
	// Every read too -- they are composed by the same hand-built strings.
	if _, err := s.GroupByID(ctx, "g-1"); err != nil {
		t.Fatalf("GroupByID: %v", err)
	}
	if _, err := s.GroupsForAccount(ctx, "acct-1"); err != nil {
		t.Fatalf("GroupsForAccount: %v", err)
	}
	if _, err := s.MembersOfGroup(ctx, "g-1"); err != nil {
		t.Fatalf("MembersOfGroup: %v", err)
	}
	if _, err := s.AccountStatus(ctx, "acct-1"); err != nil {
		t.Fatalf("AccountStatus: %v", err)
	}
	if _, err := s.UserRole(ctx, "u-1"); err != nil {
		t.Fatalf("UserRole: %v", err)
	}
	setup := &setupGraph{stubEngine: newStub()}
	setup.users["owner"] = map[string]any{"id": "owner", "role": "owner"}
	if err := New(setup, nil).ConfigureSelfAccount(SystemActorContext(ctx), awkward, "owner"); err != nil {
		t.Fatalf("ConfigureSelfAccount: %v", err)
	}
	rec.calls = append(rec.calls, setup.writes...)
	return rec.calls
}

func TestEveryComposedStatementParsesAndResolves(t *testing.T) {
	eng := realEngine(t)
	calls := productionStatements(t)
	if len(calls) == 0 {
		t.Fatal("no statements composed; this test would pass vacuously")
	}
	for _, call := range calls {
		name := constructName(call)
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s is not in the function registry: %v. A call to a construct no .memql "+
				"file declares fails at EXECUTE, which no recording engine can see.", name, err)
			continue
		}
		_, parseErr := eng.Parse(call)
		if parseErr == nil {
			continue
		}
		// Parse defaults to CLIENT origin -- the parse path is ctx-free, so
		// there is no exported way to hand it the internal origin the store
		// stamps at Execute. A @serverOnly construct therefore fails here
		// while working in production, so the assertion INVERTS for it: the
		// construct must genuinely be server-only, which is what makes the
		// store's internal-origin stamp REQUIRED rather than incidental. A
		// construct that stopped being server-only would fall out of this
		// arm and be parsed like any other.
		if serverOnlyRefusal(parseErr) {
			if !fn.ServerOnly {
				t.Errorf("%s was refused as server-only but the registry says it is not; "+
					"the parse refusal is coming from somewhere else: %v", name, parseErr)
			}
			continue
		}
		t.Errorf("does not parse through the real front end: %v\n  %s", parseErr, call)
	}
}

// serverOnlyRefusal reports the one parse refusal this package expects: a
// @serverOnly construct reached without the internal-origin stamp the store
// applies at Execute.
func serverOnlyRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is server-only and cannot be called by a client")
}

// TestTheServerOnlyReadsAreActuallyServerOnly names them, so the arm above
// cannot quietly become a way to skip a construct that broke for some other
// reason.
func TestTheServerOnlyReadsAreActuallyServerOnly(t *testing.T) {
	eng := realEngine(t)
	for _, name := range []string{"userByIdSystem", "writeGroup", "writeGroupMembership"} {
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Fatalf("%s is not in the registry: %v", name, err)
		}
		if !fn.ServerOnly {
			t.Fatalf("%s is no longer @serverOnly. Either the store may stop stamping internal "+
				"origin for it, or -- for the two writers -- an unowned row just became "+
				"client-writable, which is a primitive for putting yourself into any client's "+
				"group (epic memql#5165, D2)", name)
		}
	}
}

func TestEveryComposedArgumentIsDeclared(t *testing.T) {
	eng := realEngine(t)
	checked := 0
	for _, call := range productionStatements(t) {
		name := constructName(call)
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			continue // reported by the test above
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block, but this package passes arguments to it", name)
			continue
		}
		checked++
		for _, arg := range argNames(call) {
			if declared[arg] {
				continue
			}
			t.Errorf("%s: this package passes %q, which the construct does not declare. It is NOT "+
				"refused -- validateFunctionArgs iterates DECLARED fields -- so the value is "+
				"silently discarded and the row never receives it (memql#3626, memql#4258).",
				name, arg)
		}
	}
	if checked == 0 {
		t.Fatal("checked no call sites")
	}
}

// TestTheParseGateCanActuallyFail is the reachable positive control.
//
// Both tests above assert that nothing is wrong, and an assertion of that
// shape is worth exactly what the instrument behind it is worth.
func TestTheParseGateCanActuallyFail(t *testing.T) {
	eng := realEngine(t)
	// The retired object-literal wrapper the parser has rejected since
	// memql#2335, and the shape that broke five guest mutations.
	if _, err := eng.Parse(`mutation writeGroup({groupId: "g-1"})`); err == nil {
		t.Fatal("the front end ACCEPTED the retired object-literal call form, so " +
			"TestEveryComposedStatementParsesAndResolves proves nothing about call shape")
	}
	// A construct nothing declares must fail to resolve, or the registry
	// half is equally hollow.
	if fn, err := eng.Functions().Get("writeGroupThatDoesNotExist"); err == nil && fn != nil {
		t.Fatal("the registry resolved a construct nothing declares, so " +
			"TestEveryComposedArgumentIsDeclared proves nothing about resolution")
	}
	// And a control byte really is rejected under %q's rendering, which is
	// what makes the awkward fixture above a measurement rather than
	// decoration.
	if _, err := eng.Parse("mutation writeGroup(groupId: \"g\\x00\")"); err == nil {
		t.Fatal("the lexer ACCEPTED a \\x00 escape, so the QuoteString-vs-Go-quoting distinction " +
			"the fixture rests on is not real")
	}
}

// constructName reads the construct a rendered statement calls.
func constructName(call string) string {
	call = strings.TrimSpace(call)
	for _, prefix := range []string{"mutation ", "query "} {
		if strings.HasPrefix(call, prefix) {
			call = strings.TrimPrefix(call, prefix)
			break
		}
	}
	if i := strings.Index(call, "("); i > 0 {
		return strings.TrimSpace(call[:i])
	}
	return call
}

// argNames reads the argument names out of a rendered statement.
//
// A string scan rather than a parse, deliberately: parsing here would make the
// test agree with the parser by construction, and what it is checking is that
// the names this package CHOSE match the ones the construct declares.
func argNames(call string) []string {
	open := strings.Index(call, "(")
	if open < 0 {
		return nil
	}
	inner := call[open+1:]
	if close := strings.LastIndex(inner, ")"); close >= 0 {
		inner = inner[:close]
	}
	var out []string
	depth := 0
	inString := false
	escaped := false
	token := strings.Builder{}
	flush := func() {
		part := strings.TrimSpace(token.String())
		token.Reset()
		if i := strings.Index(part, ":"); i > 0 {
			out = append(out, strings.TrimSpace(part[:i]))
		}
	}
	for _, r := range inner {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case inString:
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
		case r == ',' && depth == 0:
			flush()
			continue
		}
		token.WriteRune(r)
	}
	flush()
	return out
}
