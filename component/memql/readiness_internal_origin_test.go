package memql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// readinessRecompute can be pulled by a cluster owner, so WriteModuleReadiness
// stamps internal origin on a REQUEST-DERIVED context. call_origin.go allows
// that only where the preconditions are asserted rather than asserted-to. This
// file is that assertion; the allowlist entry for component/memql names it.

// Precondition 1: the caller's authority does not survive the stamp. The
// context that reaches the write carries the synthetic system actor, never the
// caller's -- so nothing downstream can act as the person who asked.
// THE EVALUATION HALF OF THIS PROPERTY LIVES IN readiness_inference_test.go
// (epic memql#5118). `WriteModuleReadiness` ran the EVALUATION on the caller's
// context and only the WRITE on its own, so an owner pulling readinessRecompute
// had their own machines resolved and written as a cluster fact. The cases that
// pin it are TestTheEvaluationContextIsTheClustersOwn and
// TestTheWriteContextStaysAReader; they sit beside the inference arm they exist
// for rather than here, and this note is so a reader looking for them finds
// them.

func TestReadinessWriteCarriesNoCallerAuthority(t *testing.T) {
	caller := &auth.AccessContext{UserId: "user-42", Role: auth.RoleOwner, PrimaryEmail: "owner@example.com"}
	ctx := auth.ContextWithAccess(context.Background(), caller)
	ctx = auth.ContextWithClaims(ctx, map[string]any{"sub": "user-42", "role": "owner"})

	got, ok := auth.AccessFromContext(readinessWriteContext(ctx))
	if !ok || got == nil {
		t.Fatal("write context carries no access context")
	}
	if got.UserId == caller.UserId {
		t.Fatalf("the caller's user id survived into the write context: %q", got.UserId)
	}
	if got.UserId != "system:readiness" {
		t.Fatalf("write actor is %q, want system:readiness", got.UserId)
	}
	if got.Role == auth.RoleOwner {
		t.Fatal("the write actor holds RoleOwner; this writer needs no cluster-owner escape")
	}
	if !got.Unranked || !got.Synthetic {
		t.Fatalf("write actor must be unranked and synthetic: %+v", got)
	}
	if got.PrimaryEmail == caller.PrimaryEmail {
		t.Fatalf("the caller's email survived into the write context: %q", got.PrimaryEmail)
	}
}

// Precondition 2: the stamp is actually applied, on a context that had none.
// Without it every recordModuleReadiness call is refused by @serverOnly with
// one WARN and nothing else -- rows that never appear.
func TestReadinessWriteContextIsInternalOrigin(t *testing.T) {
	if auth.OriginFromContext(context.Background()).IsInternal() {
		t.Fatal("negative control failed: a bare context already reads as internal")
	}
	if !auth.OriginFromContext(readinessWriteContext(context.Background())).IsInternal() {
		t.Fatal("write context is not internal origin")
	}
}

// Precondition 3: no caller-supplied value reaches a readiness row. Every
// argument of the rendered mutation comes from the NodeReport, which the
// evaluator computes from the manifest and this node's own environment. This
// is what bounds the exception: a caller can ask for a recompute, and cannot
// influence what is recorded.
func TestRenderedReadinessWriteNamesOnlyTheReport(t *testing.T) {
	r := readiness.NodeReport{
		Module: "storage", NodeId: "node-a", NodeType: "bff",
		State: readiness.Partial, Core: true,
		Lanes:      []readiness.LaneReport{{Name: "azure-blob", ConfigurableFrom: "deployment", Complete: false, Slots: []readiness.SlotReport{{Name: "MEMQL_A", Present: true, Source: "env"}}}},
		ReportedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	rowId := readinessRowID(r.Module, r.NodeId)
	if rowId != "v1:platform:moduleReadiness:storage--node-a" {
		t.Fatalf("row id %q", rowId)
	}

	call, err := renderRecordModuleReadiness(r, rowId)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`rowId: "v1:platform:moduleReadiness:storage--node-a"`,
		`module: "storage"`, `nodeId: "node-a"`, `nodeType: "bff"`,
		`state: "partial"`, `core: true`, `optional: false`, `dismissable: false`, `reportedAt: "2026-09-06T12:00:00Z"`,
		`"name":"MEMQL_A"`, `"present":true`, `"source":"env"`,
	} {
		if !strings.Contains(call, want) {
			t.Errorf("rendered call is missing %s:\n%s", want, call)
		}
	}
	// The mutation named is the @serverOnly one and no other.
	if !strings.HasPrefix(call, "mutation recordModuleReadiness(") {
		t.Fatalf("rendered call does not name recordModuleReadiness:\n%s", call)
	}
}

// A report's strings go through the LEXER's escaping, not Go's. The two
// diverge on control characters, and a node id is a pod name -- attacker-shaped
// input is not the worry so much as a rendered call that will not parse.
func TestRenderedReadinessWriteEscapesThroughTheLexer(t *testing.T) {
	r := readiness.NodeReport{
		Module: `quote"and\backslash`, NodeId: "node-a", NodeType: "bff",
		State: readiness.Configured, ReportedAt: time.Unix(0, 0).UTC(),
	}
	call, err := renderRecordModuleReadiness(r, readinessRowID(r.Module, r.NodeId))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(call, `module: "quote"and`) {
		t.Fatalf("an unescaped quote reached the rendered call:\n%s", call)
	}
	if !strings.Contains(call, `\"`) {
		t.Fatalf("the quote was not escaped:\n%s", call)
	}
}

// A report with no lanes renders `lanes: []`, not `lanes: null`. A null would
// be refused by the []object arg rather than stored as an empty list, which
// would make every evaluator-backed module (ai, email) unwritable.
func TestRenderedReadinessWriteSendsAnEmptyLaneList(t *testing.T) {
	r := readiness.NodeReport{
		Module: "ai", NodeId: "node-a", NodeType: "bff",
		State: readiness.Configured, ReportedAt: time.Unix(0, 0).UTC(),
	}
	call, err := renderRecordModuleReadiness(r, readinessRowID(r.Module, r.NodeId))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(call, "lanes: []") {
		t.Fatalf("want an empty lane list:\n%s", call)
	}
}
