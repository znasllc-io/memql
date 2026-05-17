package provenance

import (
	"context"
	"strings"
	"testing"
)

func TestProvenance_Validate(t *testing.T) {
	cases := []struct {
		name    string
		p       Provenance
		wantErr string
	}{
		{"zero", Provenance{}, "kind is required"},
		{"missing name", Provenance{Kind: KindSeed}, "name is required"},
		{"unknown kind", Provenance{Kind: "weird", Name: "foo"}, "unknown kind"},
		{"valid seed", Seed("generalAssistant"), ""},
		{"valid mutation", Mutation("mutationCreateAgent"), ""},
		{"valid automation", Automation("reRouteAgent", "graph.node.created.*.v1:agents:agent"), ""},
		{"valid direct", Direct("bootstrap.insertDefaultPartition"), ""},
		{"valid system", System("systemStartup"), ""},
		{"valid migration", Migration("memqlmigrate"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestProvenance_String(t *testing.T) {
	cases := []struct {
		p    Provenance
		want string
	}{
		{Provenance{}, "<none>"},
		{Seed("generalAssistant"), "seed:generalAssistant"},
		{Seed("generalAssistant").WithVia("mutationCreateAgent"), "seed:generalAssistant via=mutationCreateAgent"},
		{Direct("frameworkInsert"), "direct:frameworkInsert"},
		{
			Automation("re", "graph.node.created.*.v1:agents:agent").WithVia("mutationUpdatePlanStatus"),
			"automation:re trigger=graph.node.created.*.v1:agents:agent via=mutationUpdatePlanStatus",
		},
	}
	for _, tc := range cases {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestContextRoundtrip(t *testing.T) {
	ctx := context.Background()

	// Empty context returns zero.
	if got := FromContext(ctx); !got.IsZero() {
		t.Errorf("empty ctx should return zero, got %+v", got)
	}

	// Round-trip through ContextWithProvenance.
	p := Seed("generalAssistant")
	ctx = ContextWithProvenance(ctx, p)
	got := FromContext(ctx)
	if got != p {
		t.Errorf("round-trip lost data: got %+v, want %+v", got, p)
	}

	// Zero provenance + ContextWithProvenance is a no-op (returns ctx
	// unchanged; caller had nothing to stamp). The retrieved value
	// stays the previously-set one.
	ctx = ContextWithProvenance(ctx, Provenance{})
	got = FromContext(ctx)
	if got != p {
		t.Errorf("zero ContextWithProvenance shouldn't overwrite; got %+v, want %+v", got, p)
	}

	// WithVia returns a new value, original is untouched.
	with := p.WithVia("mutationCreateAgent")
	if p.Via != "" {
		t.Errorf("WithVia mutated receiver; p.Via = %q", p.Via)
	}
	if with.Via != "mutationCreateAgent" {
		t.Errorf("WithVia didn't stamp Via on copy")
	}
}

func TestNilContext(t *testing.T) {
	// FromContext on nil ctx returns zero, doesn't panic.
	got := FromContext(nil)
	if !got.IsZero() {
		t.Errorf("nil ctx should return zero, got %+v", got)
	}

	// ContextWithProvenance on nil ctx returns nil, doesn't panic.
	out := ContextWithProvenance(nil, Seed("x"))
	if out != nil {
		t.Errorf("nil ctx in should return nil ctx out")
	}
}
