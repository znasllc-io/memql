package memql

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/component"
)

// engineWithRecordingLogger returns an engine whose logger writes into
// the returned buffer, so a test can assert what it SAID rather than
// only what it returned.
func engineWithRecordingLogger() (*MemQLEngine, *bytes.Buffer) {
	var buf bytes.Buffer
	return &MemQLEngine{
		Component: &component.Component{
			Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
	}, &buf
}

func conceptDeclaring(name, origin string, mirroredTo ...string) *concept.Concept {
	return &concept.Concept{Name: name, Origin: origin, MirroredTo: mirroredTo}
}

// A name no connector serves refuses, and the refusal names the concept,
// the connector, and what this build does serve.
func TestTheBootCheckRefusesAConnectorTheBuildDoesNotServe(t *testing.T) {
	e, _ := engineWithRecordingLogger()
	known := []string{"shopify"}
	isDeclared := func(n string) bool { return n == "shopify" }

	cases := []struct {
		name string
		c    *concept.Concept
	}{
		{"an origin nobody fills", conceptDeclaring("v1:x:phantom", "nowhere")},
		{"a mirror target nobody drains", conceptDeclaring("v1:x:ledger", "memql", "nowhere")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.checkConnectorRefs(collectDeclaredConnectorRefs([]*concept.Concept{tc.c}), known, isDeclared)
			if err == nil {
				t.Fatal("boot was admitted -- a mirror nobody fills reads as an empty catalog and an undrained target accumulates forever; both are silent")
			}
			for _, want := range []string{tc.c.Name, "nowhere", "shopify"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestTheBootCheckAdmitsAConnectorTheBuildServes(t *testing.T) {
	e, _ := engineWithRecordingLogger()
	all := []*concept.Concept{
		conceptDeclaring("v1:shopify:shopifyProduct", "shopify"),
		conceptDeclaring("v1:wholesale:priceList", "memql", "shopify"),
		conceptDeclaring("v1:planner:plan", ""),
	}
	err := e.checkConnectorRefs(collectDeclaredConnectorRefs(all), []string{"shopify"}, func(n string) bool { return n == "shopify" })
	if err != nil {
		t.Fatalf("boot was refused for declarations this build serves: %v", err)
	}
}

// A build that has wired no connectors CANNOT tell a typo from a correct
// name. It must not refuse -- and it must not be silent either. A gate
// that hides what it could not examine turns its own silence into a
// claim about the code.
func TestTheBootCheckAnnouncesWhenItCannotVerify(t *testing.T) {
	e, log := engineWithRecordingLogger()
	all := []*concept.Concept{
		conceptDeclaring("v1:shopify:shopifyProduct", "shopify"),
		conceptDeclaring("v1:x:ledger", "memql", "quickBooks"),
	}
	if err := e.checkConnectorRefs(collectDeclaredConnectorRefs(all), nil, nil); err != nil {
		t.Fatalf("a build with no connectors refused boot: %v -- the refusal would be a statement about the test binary's import graph, not about the tree", err)
	}
	out := log.String()
	if !strings.Contains(out, "UNVERIFIED") {
		t.Fatalf("the blind check said nothing. An operator reading this boot log would conclude the declarations were checked.\nlog: %s", out)
	}
	for _, want := range []string{"v1:shopify:shopifyProduct -> shopify", "v1:x:ledger -> quickBooks"} {
		if !strings.Contains(out, want) {
			t.Errorf("the announcement does not name %q -- it has to say WHICH declarations went unchecked, not just that some did.\nlog: %s", want, out)
		}
	}
}

// A tree that names no connectors at all is not "unverified" -- there is
// nothing to verify, and saying otherwise is noise on every boot of the
// ~124 native concepts.
func TestTheBootCheckIsSilentWhenNothingNamesAConnector(t *testing.T) {
	e, log := engineWithRecordingLogger()
	all := []*concept.Concept{conceptDeclaring("v1:planner:plan", ""), conceptDeclaring("v1:x:thing", "memql")}
	if err := e.checkConnectorRefs(collectDeclaredConnectorRefs(all), nil, nil); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if out := log.String(); strings.Contains(out, "UNVERIFIED") {
		t.Errorf("announced unverified declarations for a tree that declares none:\n%s", out)
	}
}
