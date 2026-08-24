package memoryNodes

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// buildConceptFromSource parses one concept out of src and builds it,
// returning the built Concept or the load error.
func buildConceptFromSource(t *testing.T, src string) (*Concept, error) {
	t.Helper()
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatalf("ParseFile: %v\n%s", err, src)
	}
	for _, def := range file.Definitions {
		if cd, ok := def.(*parser.ConceptDecl); ok {
			return BuildConceptFromDecl(cd, cd.Name)
		}
	}
	t.Fatalf("no concept declaration in:\n%s", src)
	return nil, nil
}

const originProbeBody = " {\n  handle string\n}\n"

// The three states, end to end from source text -- the D2 table as the
// loader sees it.
func TestConceptDataStateDerivesFromTheDeclarations(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		want       parser.DataState
		wantOrigin string
		wantMirror []string
	}{
		{
			name:       "native needs no declaration",
			src:        "concept probe" + originProbeBody,
			want:       parser.DataStateNative,
			wantOrigin: parser.OriginMemQL,
		},
		{
			name:       "explicit memql origin is still native",
			src:        "@origin(\"memql\")\nconcept probe" + originProbeBody,
			want:       parser.DataStateNative,
			wantOrigin: parser.OriginMemQL,
		},
		{
			name:       "an external origin is a mirror",
			src:        "@origin(\"shopify\")\nconcept probe" + originProbeBody,
			want:       parser.DataStateMirror,
			wantOrigin: "shopify",
		},
		{
			name:       "memql origin with a mirror target is an origin",
			src:        "@origin(\"memql\")\n@mirroredTo(\"shopify\")\nconcept probe" + originProbeBody,
			want:       parser.DataStateOrigin,
			wantOrigin: parser.OriginMemQL,
			wantMirror: []string{"shopify"},
		},
		{
			name:       "mirrored outward without an explicit origin is an origin too",
			src:        "@mirroredTo(\"shopify\", \"quickBooks\")\nconcept probe" + originProbeBody,
			want:       parser.DataStateOrigin,
			wantOrigin: parser.OriginMemQL,
			wantMirror: []string{"shopify", "quickBooks"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildConceptFromSource(t, tc.src)
			if err != nil {
				t.Fatalf("BuildConceptFromDecl: %v", err)
			}
			if got := c.DataState(); got != tc.want {
				t.Errorf("DataState() = %q, want %q", got, tc.want)
			}
			if got := c.EffectiveOrigin(); got != tc.wantOrigin {
				t.Errorf("EffectiveOrigin() = %q, want %q -- the wire value is never empty", got, tc.wantOrigin)
			}
			if len(c.MirroredTo) != len(tc.wantMirror) {
				t.Fatalf("MirroredTo = %v, want %v", c.MirroredTo, tc.wantMirror)
			}
			for i := range c.MirroredTo {
				if c.MirroredTo[i] != tc.wantMirror[i] {
					t.Errorf("MirroredTo[%d] = %q, want %q", i, c.MirroredTo[i], tc.wantMirror[i])
				}
			}
			if wantMirrorState := tc.want == parser.DataStateMirror; c.IsMirror() != wantMirrorState {
				t.Errorf("IsMirror() = %v, want %v", c.IsMirror(), wantMirrorState)
			}
		})
	}
}

// The declaration is only meaningful once, and the loader says so
// rather than letting the last one win.
func TestConceptOriginDeclarationsAreRefusedWhenRepeatedOrIncoherent(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "two origins",
			src:  "@origin(\"shopify\")\n@origin(\"quickBooks\")\nconcept probe" + originProbeBody,
			want: "exactly one origin",
		},
		{
			name: "two mirroredTo annotations",
			src:  "@mirroredTo(\"shopify\")\n@mirroredTo(\"quickBooks\")\nconcept probe" + originProbeBody,
			want: "one annotation",
		},
		{
			name: "mirroredTo on a mirror",
			src:  "@origin(\"shopify\")\n@mirroredTo(\"quickBooks\")\nconcept probe" + originProbeBody,
			want: "MIRROR",
		},
		{
			name: "malformed connector name",
			src:  "@origin(\"Shopify\")\nconcept probe" + originProbeBody,
			want: "lowerCamelCase",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConceptFromSource(t, tc.src)
			if err == nil {
				t.Fatalf("BuildConceptFromDecl accepted an incoherent declaration:\n%s", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// DeclaredConnectors is what the boot check resolves; it must name the
// origin and every target, and never "memql".
func TestDeclaredConnectorsNamesEveryConnectorTheConceptDependsOn(t *testing.T) {
	c, err := buildConceptFromSource(t,
		"@mirroredTo(\"shopify\", \"quickBooks\")\nconcept probe"+originProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	got := c.DeclaredConnectors()
	want := []string{"quickBooks", "shopify"}
	if len(got) != len(want) {
		t.Fatalf("DeclaredConnectors() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("DeclaredConnectors()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	native, err := buildConceptFromSource(t, "concept probe"+originProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	if names := native.DeclaredConnectors(); len(names) != 0 {
		t.Errorf("a native concept named connectors %v -- native data has no connector to depend on", names)
	}
}
