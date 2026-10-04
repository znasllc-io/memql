package pipelines

import (
	"strings"
	"testing"
)

// d7ExampleSpec is the design record's D7 example manifest
// (docs/superpowers/specs/2026-09-16-pipelines-program-design.md), built as
// the typed block component/packages hands this package, plus ONE line the
// record does not print: select.dbGated. D7's db-tests step says
// `only: db-gated`, and the seam's plan (decision 6) makes the db-gated trees
// a declaration, because a customer repository has no other source for them;
// the example as printed therefore refuses pipeline_select_invalid, which
// TestValidateTheRecordsExampleAsPrinted pins. The record's amendment adds
// the line.
func d7ExampleSpec() *Spec {
	return &Spec{
		Image: "ghcr.io/znasllc-io/memql-toolchain@sha256:...",
		Services: map[string]Service{
			"postgres": {Image: "ghcr.io/znasllc-io/timescaledb-pgvector@sha256:..."},
		},
		Caches: []string{"go", "npm"},
		Select: &Select{
			Go:      SelectImportGraph,
			DBGated: []string{"component/memql", "component/database"},
			Buckets: map[string][]string{
				"gates": {"**/*.md", "**/*.memql", "dsl/**", "scripts/**", "deploy/**"},
				"os":    {"clients/**", "sdk/ts/**", "brand/**"},
			},
		},
		Stages: []StageSpec{
			{Name: "checks", Steps: []StepSpec{
				{Name: "build-vet", Run: "go build ./... && go vet ./..."},
			}},
			{Name: "tests", Needs: []string{"checks"}, Steps: []StepSpec{
				{Name: "go-tests", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAffected, Shards: 4},
				{
					Name: "db-tests", Run: "MEMQL_REQUIRE_DB=1 go test $MEMQL_PACKAGES",
					Packages: PackagesAffected, Only: OnlyDBGated, Services: []string{"postgres"}, Shards: 4,
				},
				{
					Name: "os-checks", Run: "make os-typecheck os-test os-build",
					When: &When{Bucket: "os"}, Needs: map[string]bool{NeedDocker: true},
				},
			}},
			{Name: "deploy", On: []string{"push"}, Steps: []StepSpec{
				{Name: "verify-rollout", Run: "memql-verify --target=https://api.<domain> --version=$MEMQL_VERSION"},
			}},
			{Name: "notify", On: []string{"push"}, Channel: "znas-instance"},
		},
	}
}

// validateBase is the smallest spec that exercises every block a rule below
// reads; each row breaks exactly one thing in a fresh copy of it.
func validateBase() *Spec {
	return &Spec{
		Image: "example.test/toolchain@sha256:0",
		Services: map[string]Service{
			"postgres": {
				Image: "example.test/postgres@sha256:1",
				Env:   map[string]string{"POSTGRES_PASSWORD": "memql"},
				Ready: "pg_isready -U memql",
			},
		},
		Select: &Select{
			Go:      SelectImportGraph,
			DBGated: []string{"component/memql"},
			Full:    []string{"Makefile"},
			Buckets: map[string][]string{"os": {"clients/**"}},
		},
		Stages: []StageSpec{
			{Name: "checks", Steps: []StepSpec{{Name: "build", Run: "go build ./..."}}},
			{Name: "tests", Needs: []string{"checks"}, Steps: []StepSpec{
				{Name: "go-tests", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAffected, Shards: 2},
			}},
			{Name: "notify", On: []string{"push"}, Channel: "team"},
		},
	}
}

func fromValidateBase(change func(s *Spec)) func() *Spec {
	return func() *Spec {
		s := validateBase()
		change(s)
		return s
	}
}

func TestValidate(t *testing.T) {
	build := func(s *Spec) *StepSpec { return &s.Stages[0].Steps[0] } // checks/build
	goTests := func(s *Spec) *StepSpec { return &s.Stages[1].Steps[0] }

	cases := []struct {
		name  string
		spec  func() *Spec
		code  string // "" means the spec must validate
		scope string
	}{
		// Specs that must validate.
		{"the base", fromValidateBase(func(*Spec) {}), "", ""},
		{"the record's D7 example", d7ExampleSpec, "", ""},
		{"40-character names", fromValidateBase(func(s *Spec) {
			s.Stages[0].Name = strings.Repeat("a", 40)
			s.Stages[1].Needs = []string{strings.Repeat("a", 40)}
		}), "", ""},
		{"on names events and modes", fromValidateBase(func(s *Spec) {
			s.Stages[1].On = []string{"affected", "full", "pull_request", "merge_group", "push", "release"}
		}), "", ""},
		{"a step name repeats across stages", fromValidateBase(func(s *Spec) { goTests(s).Name = "build" }), "", ""},
		{"a notify stage that needs an earlier stage", fromValidateBase(func(s *Spec) {
			s.Stages[2].Needs = []string{"tests"}
		}), "", ""},
		{"a db-gated tree written ./x/", fromValidateBase(func(s *Spec) {
			s.Select.DBGated = []string{"./component/memql/"}
		}), "", ""},
		{"shards at the cap", fromValidateBase(func(s *Spec) { goTests(s).Shards = MaxShards }), "", ""},
		{"one shard without packages", fromValidateBase(func(s *Spec) { build(s).Shards = 1 }), "", ""},
		{"a need set false", fromValidateBase(func(s *Spec) { build(s).Needs = map[string]bool{NeedGPU: false} }), "", ""},
		{"timeouts at both bounds", fromValidateBase(func(s *Spec) {
			build(s).Timeout = "1m"
			goTests(s).Timeout = "2h"
		}), "", ""},

		// The whole pipeline.
		{"no pipeline block", func() *Spec { return nil }, CodeNotDeclared, ""},
		{"no stages", fromValidateBase(func(s *Spec) { s.Stages = nil }), CodeStageInvalid, ""},

		// Stages.
		{"a stage without a name", fromValidateBase(func(s *Spec) { s.Stages[1].Name = "" }), CodeStageInvalid, "stages[1]"},
		{"a stage name with capitals", fromValidateBase(func(s *Spec) { s.Stages[0].Name = "Checks" }), CodeStageInvalid, "stages[0]"},
		{"a stage name of 41 characters", fromValidateBase(func(s *Spec) { s.Stages[0].Name = strings.Repeat("a", 41) }), CodeStageInvalid, "stages[0]"},
		{"a stage name starting with a hyphen", fromValidateBase(func(s *Spec) { s.Stages[0].Name = "-checks" }), CodeStageInvalid, "stages[0]"},
		{"two stages with one name", fromValidateBase(func(s *Spec) {
			s.Stages[1].Name = "checks"
			s.Stages[1].Needs = nil
		}), CodeStageInvalid, "checks"},
		{"needs an unknown stage", fromValidateBase(func(s *Spec) { s.Stages[1].Needs = []string{"lint"} }), CodeStageInvalid, "tests"},
		{"needs itself", fromValidateBase(func(s *Spec) { s.Stages[1].Needs = []string{"tests"} }), CodeStageInvalid, "tests"},
		{"needs a later stage", fromValidateBase(func(s *Spec) { s.Stages[0].Needs = []string{"tests"} }), CodeStageInvalid, "checks"},
		{"a channel and steps", fromValidateBase(func(s *Spec) {
			s.Stages[2].Steps = []StepSpec{{Name: "post", Run: "true"}}
		}), CodeStageInvalid, "notify"},
		{"neither a channel nor steps", fromValidateBase(func(s *Spec) { s.Stages[2].Channel = "" }), CodeStageInvalid, "notify"},
		{"a channel name that is not a name", fromValidateBase(func(s *Spec) { s.Stages[2].Channel = "Team Room" }), CodeStageInvalid, "notify"},
		{"on names an unknown event", fromValidateBase(func(s *Spec) { s.Stages[2].On = []string{"pushed"} }), CodeEventUnknown, "notify"},
		{"on names a rerequest", fromValidateBase(func(s *Spec) { s.Stages[2].On = []string{"check_run"} }), CodeEventUnknown, "notify"},

		// Steps.
		{"a step without a name", fromValidateBase(func(s *Spec) { build(s).Name = "" }), CodeStepInvalid, "checks/steps[0]"},
		{"a step name with a space", fromValidateBase(func(s *Spec) { build(s).Name = "build all" }), CodeStepInvalid, "checks/steps[0]"},
		{"two steps with one name in a stage", fromValidateBase(func(s *Spec) {
			s.Stages[0].Steps = append(s.Stages[0].Steps, StepSpec{Name: "build", Run: "make"})
		}), CodeStepInvalid, "checks/build"},
		{"a step without a command", fromValidateBase(func(s *Spec) { build(s).Run = "" }), CodeStepInvalid, "checks/build"},
		{"a blank command", fromValidateBase(func(s *Spec) { build(s).Run = " \n\t" }), CodeStepInvalid, "checks/build"},
		{"packages neither affected nor all", fromValidateBase(func(s *Spec) { goTests(s).Packages = "changed" }), CodeStepInvalid, "tests/go-tests"},
		{"only that is not a db filter", fromValidateBase(func(s *Spec) { goTests(s).Only = "db" }), CodeStepInvalid, "tests/go-tests"},
		{"only without packages", fromValidateBase(func(s *Spec) { build(s).Only = OnlyDBGated }), CodeStepInvalid, "checks/build"},
		{"only db-gated with no db-gated trees", fromValidateBase(func(s *Spec) {
			s.Select.DBGated = nil
			goTests(s).Only = OnlyDBGated
		}), CodeSelectInvalid, "tests/go-tests"},
		{"only not-db-gated with no db-gated trees", fromValidateBase(func(s *Spec) {
			s.Select.DBGated = nil
			goTests(s).Only = OnlyNotDBGated
		}), CodeSelectInvalid, "tests/go-tests"},
		{"packages with no select block", fromValidateBase(func(s *Spec) { s.Select = nil }), CodeSelectMissing, "tests/go-tests"},
		{"packages with no import graph", fromValidateBase(func(s *Spec) { s.Select.Go = "" }), CodeSelectMissing, "tests/go-tests"},
		{"shards below zero", fromValidateBase(func(s *Spec) { goTests(s).Shards = -1 }), CodeStepInvalid, "tests/go-tests"},
		{"shards over the cap", fromValidateBase(func(s *Spec) { goTests(s).Shards = MaxShards + 1 }), CodeStepInvalid, "tests/go-tests"},
		{"shards without packages", fromValidateBase(func(s *Spec) { build(s).Shards = 2 }), CodeStepInvalid, "checks/build"},
		{"a bucket nobody declared", fromValidateBase(func(s *Spec) { build(s).When = &When{Bucket: "web"} }), CodeBucketUnknown, "checks/build"},
		{"a bucket with no select block", fromValidateBase(func(s *Spec) {
			s.Select = nil
			build(s).When = &When{Bucket: "os"}
		}), CodeBucketUnknown, "checks/build"},
		{"an unknown need", fromValidateBase(func(s *Spec) { build(s).Needs = map[string]bool{"network": true} }), CodeNeedUnknown, "checks/build"},
		{"an unknown need set false", fromValidateBase(func(s *Spec) { build(s).Needs = map[string]bool{"network": false} }), CodeNeedUnknown, "checks/build"},
		{"a service nobody declared", fromValidateBase(func(s *Spec) { build(s).Services = []string{"redis"} }), CodeServiceUnknown, "checks/build"},
		{"a timeout that is not a duration", fromValidateBase(func(s *Spec) { build(s).Timeout = "20" }), CodeStepInvalid, "checks/build"},
		{"a timeout under a minute", fromValidateBase(func(s *Spec) { build(s).Timeout = "59s" }), CodeStepInvalid, "checks/build"},
		{"a timeout over the cap", fromValidateBase(func(s *Spec) { build(s).Timeout = "2h1m" }), CodeStepInvalid, "checks/build"},
		{"a secret name in lower case", fromValidateBase(func(s *Spec) { build(s).Secrets = []string{"npm_token"} }), CodeSecretInvalid, "checks/build"},
		{"a secret name of 129 characters", fromValidateBase(func(s *Spec) {
			build(s).Secrets = []string{"A" + strings.Repeat("B", 128)}
		}), CodeSecretInvalid, "checks/build"},
		{"a secret named MEMQL_*", fromValidateBase(func(s *Spec) { build(s).Secrets = []string{"MEMQL_SHA"} }), CodeSecretInvalid, "checks/build"},

		// Services.
		{"a service without an image", fromValidateBase(func(s *Spec) {
			s.Services["postgres"] = Service{Env: map[string]string{"POSTGRES_PASSWORD": "memql"}}
		}), CodeStepInvalid, "services/postgres"},
		{"a service env name that is not an env name", fromValidateBase(func(s *Spec) {
			s.Services["postgres"] = Service{Image: "example.test/postgres@sha256:1", Env: map[string]string{"pg-pass": "x"}}
		}), CodeStepInvalid, "services/postgres"},
		{"a service name that is not a name", fromValidateBase(func(s *Spec) {
			s.Services["Postgres DB"] = Service{Image: "example.test/postgres@sha256:1"}
		}), CodeStepInvalid, "services"},

		// Select.
		{"select.go that is not the import graph", fromValidateBase(func(s *Spec) { s.Select.Go = "go-list" }), CodeSelectInvalid, "select"},
		{"a full glob outside the grammar", fromValidateBase(func(s *Spec) { s.Select.Full = []string{"src/{a,b}/**"} }), CodeSelectInvalid, "select/full"},
		{"an empty full glob", fromValidateBase(func(s *Spec) { s.Select.Full = []string{""} }), CodeSelectInvalid, "select/full"},
		{"a bucket glob outside the grammar", fromValidateBase(func(s *Spec) {
			s.Select.Buckets["os"] = []string{"clients/[ab]/**"}
		}), CodeSelectInvalid, "select/buckets/os"},
		{"a bucket with no globs", fromValidateBase(func(s *Spec) { s.Select.Buckets["os"] = nil }), CodeSelectInvalid, "select/buckets/os"},
		{"a bucket name that is not a name", fromValidateBase(func(s *Spec) {
			s.Select.Buckets["OS"] = []string{"clients/**"}
		}), CodeSelectInvalid, "select/buckets"},
		{"an absolute db-gated tree", fromValidateBase(func(s *Spec) { s.Select.DBGated = []string{"/component/memql"} }), CodeSelectInvalid, "select/dbGated"},
		{"a db-gated tree climbing out", fromValidateBase(func(s *Spec) { s.Select.DBGated = []string{"../memql"} }), CodeSelectInvalid, "select/dbGated"},
		{"a db-gated Go wildcard", fromValidateBase(func(s *Spec) { s.Select.DBGated = []string{"component/memql/..."} }), CodeSelectInvalid, "select/dbGated"},
		{"an empty db-gated tree", fromValidateBase(func(s *Spec) { s.Select.DBGated = []string{""} }), CodeSelectInvalid, "select/dbGated"},
		{"a db-gated tree written ./", fromValidateBase(func(s *Spec) { s.Select.DBGated = []string{"./"} }), CodeSelectInvalid, "select/dbGated"},
		{"a db-gated tree written as a glob", fromValidateBase(func(s *Spec) { s.Select.DBGated = []string{"component/**"} }), CodeSelectInvalid, "select/dbGated"},
	}

	// Several rules share a code AND a scope -- a stage needing an unknown
	// stage, itself or a later one; the three timeout rules -- so code and
	// scope alone would stay green with one of them deleted. The detail tells
	// them apart.
	details := map[string]string{
		"a stage without a name":                     "has no name",
		"a stage name with capitals":                 "is not a name",
		"a stage name of 41 characters":              "is not a name",
		"a stage name starting with a hyphen":        "is not a name",
		"two stages with one name":                   "Two stages are named",
		"needs an unknown stage":                     "is not a stage of this pipeline",
		"needs itself":                               "needs itself",
		"needs a later stage":                        "comes after it",
		"a channel and steps":                        "both a channel and steps",
		"neither a channel nor steps":                "neither steps nor a channel",
		"a channel name that is not a name":          `names channel "Team Room"`,
		"a step without a name":                      "has no name",
		"a step name with a space":                   "is not a name",
		"two steps with one name in a stage":         "two steps named",
		"a step without a command":                   "no command to run",
		"a blank command":                            "no command to run",
		"packages neither affected nor all":          `packages is "changed"`,
		"only that is not a db filter":               `only is "db"`,
		"only without packages":                      "selects none",
		"only db-gated with no db-gated trees":       "only: db-gated needs select.dbGated",
		"only not-db-gated with no db-gated trees":   "only: not-db-gated needs select.dbGated",
		"packages with no select block":              "no select: block",
		"packages with no import graph":              "does not name the import graph",
		"shards below zero":                          "shards is -1",
		"shards over the cap":                        "shards is 9",
		"shards without packages":                    "no packages to split",
		"a bucket nobody declared":                   `when.bucket is "web"`,
		"a timeout that is not a duration":           "is not a duration",
		"a timeout under a minute":                   "under the 1m minimum",
		"a timeout over the cap":                     "over the 2h maximum",
		"a secret name in lower case":                "is not a secret name",
		"a secret name of 129 characters":            "is not a secret name",
		"a secret named MEMQL_*":                     "reserved",
		"a service without an image":                 "names no image",
		"a service env name that is not an env name": `sets "pg-pass"`,
		"a service name that is not a name":          `Service name "Postgres DB"`,
		"select.go that is not the import graph":     `select.go is "go-list"`,
		"a bucket with no globs":                     "lists no paths",
		"an empty db-gated tree":                     "not a directory that can hold a Go package",
		"a db-gated tree written as a glob":          "not a directory that can hold a Go package",
	}
	used := map[string]bool{}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(tc.spec())
			if tc.code == "" {
				if r != nil {
					t.Fatalf("Validate refused a spec that must validate: %v", r)
				}
				return
			}
			if r == nil {
				t.Fatalf("Validate accepted it; want %s (%q)", tc.code, tc.scope)
			}
			if r.Code != tc.code || r.Scope != tc.scope {
				t.Errorf("Validate = %s (%q), want %s (%q); detail: %s", r.Code, r.Scope, tc.code, tc.scope, r.Detail)
			}
			if strings.TrimSpace(r.Detail) == "" {
				t.Errorf("%s carries no detail; the detail is the sentence a person reads", r.Code)
			}
			if want, ok := details[tc.name]; ok {
				used[tc.name] = true
				if !strings.Contains(r.Detail, want) {
					t.Errorf("detail %q does not say %q, so another rule refused it", r.Detail, want)
				}
			}
			if class, _ := ClassOf(r.Code); class != ClassRefusal {
				t.Errorf("%s is class %q; a spec that cannot compile is a refusal", r.Code, class)
			}
		})
	}
	for name := range details {
		if !used[name] {
			t.Errorf("details names %q, which is no refusing row: the check it carries never ran", name)
		}
	}
}

// The record's example exactly as printed: `only: db-gated` with no
// select.dbGated has nothing to intersect with. This is the line the record's
// amendment adds; if the rule ever relaxes, d7ExampleSpec's comment is stale.
func TestValidateTheRecordsExampleAsPrinted(t *testing.T) {
	spec := d7ExampleSpec()
	spec.Select.DBGated = nil
	r := Validate(spec)
	if r == nil || r.Code != CodeSelectInvalid || r.Scope != "tests/db-tests" {
		t.Errorf("Validate(D7 as printed) = %v, want pipeline_select_invalid (tests/db-tests)", r)
	}
}

// An unknown need is refused with the closed set in words, so the person
// reading the check run learns what they may write.
func TestValidateNamesTheClosedSetOfNeeds(t *testing.T) {
	spec := validateBase()
	spec.Stages[0].Steps[0].Needs = map[string]bool{"network": true, "docker": true}
	r := Validate(spec)
	if r == nil || r.Code != CodeNeedUnknown {
		t.Fatalf("Validate = %v, want pipeline_need_unknown", r)
	}
	if !strings.Contains(r.Detail, `"network"`) {
		t.Errorf("detail %q does not name the unknown need", r.Detail)
	}
	for _, need := range Needs() {
		if !strings.Contains(r.Detail, need) {
			t.Errorf("detail %q does not list %q from the closed set", r.Detail, need)
		}
	}
}

// Validate is a question, not a repair: it must leave the spec as it found it.
func TestValidateDoesNotChangeTheSpec(t *testing.T) {
	spec := validateBase()
	spec.Select.DBGated = []string{"./component/memql/"}
	if r := Validate(spec); r != nil {
		t.Fatalf("Validate: %v", r)
	}
	if got := spec.Select.DBGated[0]; got != "./component/memql/" {
		t.Errorf("Validate rewrote select.dbGated to %q", got)
	}
}
