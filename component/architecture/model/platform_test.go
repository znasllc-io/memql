package model

import (
	"bytes"
	"strings"
	"testing"
)

func platformFixture() *PlatformGraph {
	return &PlatformGraph{
		SchemaVersion: PlatformSchemaVersion,
		Nodes: []PlatformNode{
			{ID: HostID("api.<domain>"), Kind: PlatformHost, Label: "api.<domain>", Attrs: map[string]string{"role": "api"}},
			{ID: ServiceID("agent"), Kind: PlatformRole, Label: "agent", Attrs: map[string]string{"mesh": "true"}},
			{ID: DeploymentID("agent"), Kind: PlatformDeployment, Label: "agent"},
		},
		Edges: []PlatformEdge{
			{From: ServiceID("agent"), To: DeploymentID("agent"), Kind: PlatformRunsOn},
		},
	}
}

// One canonical byte form: sorted, one element per line, <domain> unescaped,
// and the same bytes however the input was ordered.
func TestPlatformGraphWriteJSONIsCanonical(t *testing.T) {
	g := platformFixture()
	var a bytes.Buffer
	if err := g.WriteJSON(&a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.String(), `"host:api.<domain>"`) {
		t.Errorf("the placeholder was escaped:\n%s", a.String())
	}
	if lines := strings.Count(a.String(), "\n"); lines != 3+1+2+1 {
		t.Errorf("want one line per element plus the frame, got %d lines:\n%s", lines, a.String())
	}

	reversed := platformFixture()
	reversed.Nodes[0], reversed.Nodes[2] = reversed.Nodes[2], reversed.Nodes[0]
	var b bytes.Buffer
	if err := reversed.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("the byte form depends on the input order")
	}
	if reversed.Nodes[0].ID != DeploymentID("agent") {
		t.Error("WriteJSON reordered its receiver")
	}

	back, err := ReadPlatformGraph(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !back.IsSorted() || len(back.Nodes) != 3 || len(back.Edges) != 1 {
		t.Fatalf("round trip = %+v", back)
	}
	if err := ValidatePlatformGraph(back); err != nil {
		t.Fatal(err)
	}
}

func TestReadPlatformGraphIsStrict(t *testing.T) {
	if _, err := ReadPlatformGraph(strings.NewReader(`{"schema_version":"0","nodes":[],"edges":[]}`)); err == nil {
		t.Error("a different schema version was accepted")
	}
	if _, err := ReadPlatformGraph(strings.NewReader(`{"schema_version":"1","nodes":[],"edges":[],"extra":1}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
}

func TestValidatePlatformGraph(t *testing.T) {
	dup := platformFixture()
	dup.Nodes = append(dup.Nodes, dup.Nodes[0])
	dangling := platformFixture()
	dangling.Edges = append(dangling.Edges, PlatformEdge{From: ServiceID("agent"), To: DeploymentID("gone"), Kind: PlatformRunsOn})
	badKind := platformFixture()
	badKind.Nodes[0].Kind = "gadget"
	badEdge := platformFixture()
	badEdge.Edges[0].Kind = "likes"
	for name, g := range map[string]*PlatformGraph{"duplicate": dup, "dangling": dangling, "node kind": badKind, "edge kind": badEdge} {
		if err := ValidatePlatformGraph(g); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
