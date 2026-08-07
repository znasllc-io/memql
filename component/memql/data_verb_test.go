package memql

import (
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// classifierEngine builds a DB-less engine carrying the full embedded DSL
// tree. Parse (and therefore DataVerbFor) needs the loaded function registry,
// not a database -- see TestEngineInitLoadsFullDSL, which boots the same way.
func classifierEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

// TestDataVerbFor classifies REAL constructs out of the shipped DSL tree --
// not synthetic fixtures -- so the classifier is pinned to the grammar the
// request path actually receives.
func TestDataVerbFor(t *testing.T) {
	eng := classifierEngine(t)

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{
			name: "bare concept filter is a read",
			// The literal query component/node/bootstrap.go:105 issues.
			query: "concept==v1:cluster:node",
			want:  auth.VerbRead,
		},
		{
			name:  "named query call is a read",
			query: `agentById(agentId: "abc")`,
			want:  auth.VerbRead,
		},
		{
			name:  "named mutation call is a write",
			query: `createAgent(agentId: "abc", ownerUserId: "v1:identity:user:u1", name: "X")`,
			want:  auth.VerbCreate,
		},
		{
			name:  "insert literal is a write",
			query: `insert("v1:cluster:node", id="gate-3179-n1", payload={"active":true})`,
			want:  auth.VerbCreate,
		},
		{
			name:  "empty query classifies read (Execute reports the real error)",
			query: "",
			want:  auth.VerbRead,
		},
		{
			name:  "unparseable query classifies read (Execute reports the parse error)",
			query: "!!! not memql !!!",
			want:  auth.VerbRead,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eng.DataVerbFor(t.Context(), tc.query); got != tc.want {
				t.Fatalf("DataVerbFor(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestDataVerbForIsClassificationOnly pins the boundary the #3179 ruling drew:
// the classifier answers a question, it never refuses. Engine.Execute stays
// ungated, which is what keeps node bootstrap (component/node/bootstrap.go:105,
// Engine.Execute on a context.Background() with no AccessContext at all) off
// the enforcement path without needing an enumerated bypass.
func TestDataVerbForIsClassificationOnly(t *testing.T) {
	eng := classifierEngine(t)

	// Precondition: reader genuinely lacks create-on-data, so if Execute were
	// gated this actor-less call would be the one to fail.
	if auth.Capable(auth.RoleReader, auth.VerbCreate, auth.ResourceData) {
		t.Fatal("precondition: reader must not hold create-on-data")
	}

	// The bootstrap call shape, verbatim: no access context, no claims.
	_, err := eng.Execute(t.Context(), "concept==v1:cluster:node")
	if err == nil {
		t.Skip("nil-DB engine unexpectedly executed; nothing to assert")
	}
	// The nil-DB engine must fail on the DATABASE, never on authorization.
	got := strings.ToLower(err.Error())
	for _, marker := range []string{"permission", "capability", "not authorized", "forbidden"} {
		if strings.Contains(got, marker) {
			t.Fatalf("Engine.Execute refused an actor-less call on authorization grounds: %v", err)
		}
	}
}

// TestDataVerbForClassifiesEveryDSLMutationAsAWrite sweeps the whole loaded
// function registry instead of a handful of hand-picked names, so a mutation
// shape the classifier does not recognise shows up here rather than in
// production. Mutations whose required-arg validation rejects a bare `name()`
// call are skipped -- there is nothing to classify when the call cannot parse
// at all -- and the sweep fails if that leaves nothing checked.
func TestDataVerbForClassifiesEveryDSLMutationAsAWrite(t *testing.T) {
	eng := classifierEngine(t)
	ctx := t.Context()

	var checked int
	var misread []string
	for name, fn := range eng.Functions().Snapshot() {
		if fn == nil || !strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "mutation") {
			continue
		}
		call := name + "()"
		if _, err := eng.parseWithFunctionsAmbient(call, eng.functions, nil, false, auth.OriginClient, nil); err != nil {
			continue
		}
		checked++
		if got := eng.DataVerbFor(ctx, call); got != auth.VerbCreate {
			misread = append(misread, name+" -> "+got)
		}
	}
	if checked == 0 {
		t.Fatal("no DSL mutation parsed as a bare call; the sweep proved nothing")
	}
	if len(misread) > 0 {
		sort.Strings(misread)
		t.Fatalf("%d/%d DSL mutations classified as reads: %v", len(misread), checked, misread)
	}
	t.Logf("classified %d DSL mutations as writes", checked)
}
