package conformance

// editor_load_parity_test.go -- the editor says what the load says
// (memql#5434).
//
// Sense's load pass shows Lower's load refusals in the editor. This holds it to
// the corpus: every case the load refuses with a Lower rule id is refused by
// the pass too -- same id, same words -- when the case file is the document an
// author has open in its directory's domain, the fixture beside it on disk.
//
// A case whose refused construct is an automation is left out, and that is a
// stated boundary rather than a gap in the reading: a trigger filter is checked
// by the automations preparer, which re-parses the filter from compiled text
// and cannot say where in the author's file the refused node is, so the editor
// pass does not compile automations (component/memql/sense_load_pass.go).

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
)

func TestCorpusLowerRefusalsReachTheEditor(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "") // strict, as a node boots
	var cases []*corpusRun
	skippedAutomations := 0
	for _, r := range discoverCorpus(t) {
		if r.c.Verdict != verdictRefuseLoad || !strings.HasPrefix(r.c.Code, "lower_") {
			continue
		}
		if corpusAutomationDecl.MatchString(r.src) {
			skippedAutomations++
			continue
		}
		cases = append(cases, r)
	}
	// A floor, so a filter that stops matching reads as a failure rather than
	// as a corpus with nothing to hold the pass to.
	if len(cases) < 10 {
		t.Fatalf("expected the corpus's Lower refusals (13 when this was written), found %d", len(cases))
	}
	t.Logf("%d Lower refusals held to the editor pass; %d automation cases left to the load", len(cases), skippedAutomations)

	// One domain per case directory, holding that directory's fixture: the
	// workspace the author is in. The case file is not on disk -- it is the
	// open buffer.
	tree := fstest.MapFS{}
	domainOf := map[string]string{}
	for _, r := range cases {
		domain := corpusDomainName(strings.TrimPrefix(r.dir, r.edition+"/"))
		domainOf[r.dir] = domain
		tree[domain+"/"+dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(r.line.Render())}
		if r.fixture != "" {
			tree[domain+"/fixture.memql"] = &fstest.MapFile{Data: []byte(r.fixture)}
		}
		for name, data := range r.sidecars {
			tree[domain+"/"+name] = &fstest.MapFile{Data: data}
		}
	}
	svc, err := memql.BuildOfflineSense(tree)
	if err != nil {
		t.Fatalf("the fixtures of the refusing directories must load clean together: %v", err)
	}
	if !svc.CanLoad() {
		t.Fatal("the built service has no engine to run the load with")
	}

	for _, r := range cases {
		r := r
		t.Run(r.name(), func(t *testing.T) {
			got := svc.DiagnoseLoad(context.Background(), r.src, domainOf[r.dir]+"/case.memql")
			for _, d := range got {
				if d.Code == r.c.Code && strings.Contains(d.Message, r.c.Message) {
					if d.Range.Start.Line < 1 || d.Range.Start.Column < 1 {
						t.Errorf("the refusal is unplaced: %+v", d)
					}
					return
				}
			}
			t.Errorf("the load refuses %s with %s (%q); the editor pass showed %+v", r.rel, r.c.Code, r.c.Message, got)
		})
	}
}

// The other half: no false refusal. Every case the load accepts draws no
// refusal from the editor pass, over the same workspace -- every accepting
// directory's fixture in its own domain, the case as the open buffer.
func TestCorpusLoadsDrawNoEditorRefusal(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "") // strict, as a node boots
	var cases []*corpusRun
	for _, r := range discoverCorpus(t) {
		if r.c.Verdict != verdictLoadOK || corpusAutomationDecl.MatchString(r.src) {
			continue
		}
		cases = append(cases, r)
	}
	if len(cases) < 100 {
		t.Fatalf("expected the corpus's accepting cases (hundreds), found %d", len(cases))
	}

	tree := fstest.MapFS{}
	domainOf := map[string]string{}
	for _, r := range cases {
		domain := corpusDomainName(strings.TrimPrefix(r.dir, r.edition+"/"))
		domainOf[r.dir] = domain
		tree[domain+"/"+dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(r.line.Render())}
		if r.fixture != "" {
			tree[domain+"/fixture.memql"] = &fstest.MapFile{Data: []byte(r.fixture)}
		}
		for name, data := range r.sidecars {
			tree[domain+"/"+name] = &fstest.MapFile{Data: data}
		}
	}
	svc, err := memql.BuildOfflineSense(tree)
	if err != nil {
		t.Fatalf("the accepting directories' fixtures must load clean together: %v", err)
	}
	if !svc.CanLoad() {
		t.Fatal("the built service has no engine to run the load with")
	}
	refused := 0
	for _, r := range cases {
		if got := svc.DiagnoseLoad(context.Background(), r.src, domainOf[r.dir]+"/case.memql"); len(got) != 0 {
			refused++
			t.Errorf("%s loads clean, yet the editor pass refused it: %+v", r.rel, got)
		}
	}
	t.Logf("%d accepting cases, %d refused by the editor pass", len(cases), refused)
}
