package client

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The data-origins fields survive the wire -> SDK projection (epic
// memql#4378). They are the fields a client reads BEFORE offering an
// edit: a concept whose DataState is "mirror" refuses every write that
// is not its connector's, so an editor rendered over one offers an
// action the server will refuse.
func TestConceptsFromProtoCarryTheDataOriginsDeclaration(t *testing.T) {
	in := []*memqlv1.ConceptInfo{
		{
			Id:             "v1:shopify:product",
			DataState:      "mirror",
			DataOrigin:     "shopify",
			DataMirroredTo: nil,
		},
		{
			Id:             "v1:wholesale:priceList",
			DataState:      "origin",
			DataOrigin:     "memql",
			DataMirroredTo: []string{"shopify", "quickBooks"},
		},
		{
			Id:         "v1:planner:plan",
			DataState:  "native",
			DataOrigin: "memql",
		},
	}

	out := conceptsFromProto(in)
	if len(out) != len(in) {
		t.Fatalf("conceptsFromProto returned %d concepts, want %d", len(out), len(in))
	}

	if out[0].DataState != "mirror" || out[0].DataOrigin != "shopify" {
		t.Errorf("mirror: DataState=%q DataOrigin=%q, want \"mirror\"/\"shopify\"", out[0].DataState, out[0].DataOrigin)
	}
	if len(out[0].DataMirroredTo) != 0 {
		t.Errorf("mirror: DataMirroredTo=%v, want empty -- only an ORIGIN has mirror targets", out[0].DataMirroredTo)
	}

	if out[1].DataState != "origin" {
		t.Errorf("origin: DataState=%q, want \"origin\"", out[1].DataState)
	}
	if len(out[1].DataMirroredTo) != 2 || out[1].DataMirroredTo[0] != "shopify" || out[1].DataMirroredTo[1] != "quickBooks" {
		t.Errorf("origin: DataMirroredTo=%v, want [shopify quickBooks] in authored order", out[1].DataMirroredTo)
	}

	if out[2].DataState != "native" || out[2].DataOrigin != "memql" {
		t.Errorf("native: DataState=%q DataOrigin=%q, want \"native\"/\"memql\" -- "+
			"the server sends the EFFECTIVE origin so no client re-derives the default",
			out[2].DataState, out[2].DataOrigin)
	}
}

// A server that predates the fields leaves them empty, and the SDK says
// so rather than inventing a state.
func TestConceptsFromProtoLeavesDataOriginsEmptyWhenTheServerSendsNone(t *testing.T) {
	out := conceptsFromProto([]*memqlv1.ConceptInfo{{Id: "v1:planner:plan"}})
	if len(out) != 1 {
		t.Fatalf("conceptsFromProto returned %d concepts, want 1", len(out))
	}
	if out[0].DataState != "" {
		t.Errorf("DataState=%q, want \"\" -- an older server said nothing and the SDK must not guess \"native\"",
			out[0].DataState)
	}
	if out[0].DataOrigin != "" {
		t.Errorf("DataOrigin=%q, want \"\"", out[0].DataOrigin)
	}
}
