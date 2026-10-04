package packages

import (
	"encoding/json"
	"strings"
	"testing"
)

// manifest_pipeline_test.go -- the pipeline: block of memql-package.yaml (epic
// memql#5477, design record D3 and D7-D9).
//
// The block is read by the ONE strict decoder every manifest goes through, so
// its SHAPE is held to the same rule as any other key: an unknown key is
// package_manifest_invalid. What the block MEANS is not this package's
// question. A need nobody offers, a timeout that is not a duration, a bucket
// nobody declared -- those fail the pipeline's run with a typed refusal (D9)
// and must never refuse a deploy of the source they sit in.

// pipelineManifest is the D7 example from the design record, beside one
// deployable so the manifest is also an ordinary package.
const pipelineManifest = `formatVersion: 1
name: memql
deployables:
  - name: docs
    path: clients/docs
    kind: static
pipeline:
  image: ghcr.io/znasllc-io/memql-toolchain@sha256:abc
  services:
    postgres:
      image: ghcr.io/znasllc-io/timescaledb-pgvector@sha256:def
      env:
        POSTGRES_PASSWORD: memql
      ready: pg_isready -U postgres
  caches: [go, npm]
  select:
    go: import-graph
    dbGated: [component/memql, component/database]
    full: ["Makefile"]
    buckets:
      gates: ["**/*.md", "**/*.memql", "dsl/**", "scripts/**", "deploy/**"]
      os:    ["clients/**", "sdk/ts/**", "brand/**"]
  stages:
    - name: checks
      steps:
        - name: build-vet
          run: go build ./... && go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: go-tests
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 4
        - name: db-tests
          run: MEMQL_REQUIRE_DB=1 go test $MEMQL_PACKAGES
          packages: affected
          only: db-gated
          services: [postgres]
          shards: 4
          timeout: 20m
        - name: os-checks
          run: make os-typecheck os-test os-build
          when: { bucket: os }
          needs: { docker: true }
    - name: deploy
      on: [push]
      steps:
        - name: verify-rollout
          run: memql-verify --target=https://api.<domain> --version=$MEMQL_VERSION
          secrets: [VERIFY_TOKEN]
    - name: notify
      on: [push]
      channel: znas-instance
`

func TestThePipelineBlockParsesIntoTheSpec(t *testing.T) {
	m, err := ParseManifest([]byte(pipelineManifest))
	if err != nil {
		t.Fatalf("the design record's own example must parse: %v", err)
	}
	p := m.Pipeline
	if p == nil {
		t.Fatal("a manifest with a pipeline: block must carry it on Pipeline")
	}
	if p.Image != "ghcr.io/znasllc-io/memql-toolchain@sha256:abc" {
		t.Fatalf("image: %q", p.Image)
	}
	pg, ok := p.Services["postgres"]
	if !ok || pg.Image == "" || pg.Env["POSTGRES_PASSWORD"] != "memql" || pg.Ready == "" {
		t.Fatalf("the postgres service lost a field: %+v", p.Services)
	}
	if strings.Join(p.Caches, ",") != "go,npm" {
		t.Fatalf("caches: %v", p.Caches)
	}
	if p.Select == nil || p.Select.Go != "import-graph" || len(p.Select.DBGated) != 2 ||
		len(p.Select.Full) != 1 || len(p.Select.Buckets["gates"]) != 5 || len(p.Select.Buckets["os"]) != 3 {
		t.Fatalf("select: %+v", p.Select)
	}

	var names []string
	for _, s := range p.Stages {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "checks,tests,deploy,notify" {
		t.Fatalf("stages, in the order written: %v", names)
	}
	tests := p.Stages[1]
	if strings.Join(tests.Needs, ",") != "checks" || len(tests.Steps) != 3 {
		t.Fatalf("the tests stage: %+v", tests)
	}
	db := tests.Steps[1]
	if db.Packages != "affected" || db.Only != "db-gated" || db.Shards != 4 ||
		strings.Join(db.Services, ",") != "postgres" || db.Timeout != "20m" {
		t.Fatalf("the db-tests step: %+v", db)
	}
	osChecks := tests.Steps[2]
	if osChecks.When == nil || osChecks.When.Bucket != "os" || !osChecks.Needs["docker"] {
		t.Fatalf("the os-checks step: %+v", osChecks)
	}
	if deploy := p.Stages[2]; strings.Join(deploy.On, ",") != "push" ||
		strings.Join(deploy.Steps[0].Secrets, ",") != "VERIFY_TOKEN" {
		t.Fatalf("the deploy stage: %+v", deploy)
	}
	if notify := p.Stages[3]; notify.Channel != "znas-instance" || len(notify.Steps) != 0 {
		t.Fatalf("the notify stage: %+v", notify)
	}

	// The deployables beside it are untouched by the block.
	if len(m.Deployables) != 1 || m.Deployables[0].Name != "docs" {
		t.Fatalf("deployables: %+v", m.Deployables)
	}
}

// TestASemanticMistakeInThePipelineStillParses is Review Focus 4's parse
// half: a need nobody offers, a timeout that is no duration and a bucket
// nobody declared are the PIPELINE's refusals, raised when a run compiles.
// Here they are values like any other, and the manifest parses.
func TestASemanticMistakeInThePipelineStillParses(t *testing.T) {
	src := strings.Replace(pipelineManifest, "needs: { docker: true }", "needs: { quantum: true }", 1)
	src = strings.Replace(src, "timeout: 20m", "timeout: twenty minutes", 1)
	src = strings.Replace(src, "when: { bucket: os }", "when: { bucket: nobody-declared-this }", 1)
	m, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("a semantic mistake in the pipeline must not refuse the manifest: %v", err)
	}
	steps := m.Pipeline.Stages[1].Steps
	if !steps[2].Needs["quantum"] || steps[1].Timeout != "twenty minutes" || steps[2].When.Bucket != "nobody-declared-this" {
		t.Fatalf("the mistakes must arrive as written, for the compile to refuse by name: %+v", steps)
	}
}

// TestAnUnknownKeyInsideThePipelineIsInvalid: the block's SHAPE is the
// manifest's shape, so a misspelled key anywhere inside it refuses exactly as
// `deployabels:` does. Each case is the valid manifest with one key changed,
// and the valid manifest parsing (above) is the reachable positive.
func TestAnUnknownKeyInsideThePipelineIsInvalid(t *testing.T) {
	// {text to replace, its replacement, the unknown key the refusal names}
	for name, edit := range map[string][3]string{
		"in the block":   {"  caches: [go, npm]\n", "  cachez: [go, npm]\n", "cachez"},
		"in a service":   {"      ready: pg_isready -U postgres\n", "      readiness: pg_isready -U postgres\n", "readiness"},
		"in select":      {"    go: import-graph\n", "    golang: import-graph\n", "golang"},
		"in a stage":     {"    - name: tests\n      needs: [checks]\n", "    - name: tests\n      after: [checks]\n", "after"},
		"in a step":      {"          shards: 4\n          timeout: 20m\n", "          shardz: 4\n          timeout: 20m\n", "shardz"},
		"in a step when": {"when: { bucket: os }", "when: { buckets: os }", "buckets"},
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(pipelineManifest, edit[0], edit[1], 1)
			if src == pipelineManifest {
				t.Fatalf("the edit %q matched nothing, so this case would test the valid manifest", edit[0])
			}
			_, err := ParseManifest([]byte(src))
			if got := RefusalCode(err); got != CodeManifestInvalid {
				t.Fatalf("want %s, got %q (%v)", CodeManifestInvalid, got, err)
			}
			// Refused for the KEY, not for something the edit broke on the way.
			if !strings.Contains(err.Error(), "field "+edit[2]+" not found") {
				t.Fatalf("the refusal must name the unknown key %q: %v", edit[2], err)
			}
		})
	}
}

// A manifest with no pipeline: block has no Pipeline, and says nothing about
// one when it travels -- the report a deploy lands on its row is unchanged for
// every package written before pipelines existed.
func TestAManifestWithoutAPipelineCarriesNone(t *testing.T) {
	m, err := ParseManifest([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.Pipeline != nil {
		t.Fatalf("no block, and a Pipeline anyway: %+v", m.Pipeline)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), `"pipeline"`) {
		t.Fatalf("an absent block must not appear on the wire: %s", raw)
	}

	// THE CONTROL: the same check sees the key when the block is there, so
	// the silence above is about the manifest and not about the marshal.
	withBlock, err := ParseManifest([]byte(pipelineManifest))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(withBlock)
	if !strings.Contains(string(raw), `"pipeline"`) || !strings.Contains(string(raw), `"stages"`) {
		t.Fatalf("a declared block must travel with the manifest: %s", raw)
	}
}
