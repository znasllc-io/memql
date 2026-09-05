package app

import (
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/dsl"
)

// The startup payload is a Go↔DSL SEAM, and this is the gate on it (memql#4926).
//
// # What went wrong
//
// `parseDatabaseInfo` put the DSN's port on the payload as the string
// `url.URL.Port()` hands back. `bootstrapCluster` forwards it to
// `createDatabase`, which declares `port int`, so every bff start logged
//
//	argument "port": expected int, got string
//
// and the run stopped at that step -- taking `idpRecord` and the cluster
// cross-link with it. The `v1:cluster:database:primary` singleton is meant to
// refresh on every bff start (memql#4766) and had never refreshed on any
// cluster whose DSN names a port, which is all of them.
//
// # Why nothing caught it
//
// Three suites cover this path and each misses for its own reason. The
// topology-row db test passes `port: 15432` as a bare integer literal in
// hand-written MemQL, so it proves the mutation stores an int and nothing
// about what the automation sends. The automations package keeps its own
// inline copy of the automation that has no `port` argument at all.
// `TestBootstrapClusterForwardsEveryDeclaredField` compares argument NAMES on
// both sides and never looks at a type, so it confirmed `port` was forwarded
// while the value being forwarded could not be accepted.
//
// The common blind spot is that no test ever held a payload VALUE against the
// arg it is forwarded TO. That is what this does.
//
// # The rule being enforced, and why it is stricter than the engine validator
//
// A step does not hand the engine a Go value. It renders its resolved
// arguments back into MemQL SOURCE TEXT (`renderMemQLValue`,
// component/automations/steps/function.go) and the engine parses that. A Go
// string is rendered QUOTED, so it reaches argument validation as a string
// literal and can never satisfy a numeric arg -- however numeric its digits
// look. `component/memql/function_validator.go` does admit a whole float64 for
// an `int`, because a float renders as a bare numeric literal; it does not
// admit a string, and no `??` default can rescue one: `??` falls through on
// absent or blank, and "5432" is neither.
//
// So the admissible set AT THIS SEAM is narrower than the validator's alone,
// and stating it here is the point rather than a duplicate of it.

// declaredArgTypes reads one mutation's `args { }` block out of the DSL source
// and returns declared name -> declared type.
//
// Read from the tree at test time rather than kept as a literal here: a
// hand-copied list is a list that agrees with a stale reading of the DSL, which
// is the failure mode of the automations package's inline copy of this same
// automation.
func declaredArgTypes(t *testing.T, path, mutation string) map[string]string {
	t.Helper()

	f, err := dsl.Tree().Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	block := regexp.MustCompile(
		`(?s)mutate\s+\w+\s+` + regexp.QuoteMeta(mutation) + `\s*\{.*?args\s*\{(.*?)\n  \}`,
	).FindStringSubmatch(string(body))
	if block == nil {
		t.Fatalf("%s: no args block found for %q -- the mutation was renamed or reshaped, "+
			"and this gate would otherwise pass while examining nothing", path, mutation)
	}

	// One declared field: `    name  type [annotations]`, skipping `///` docs.
	decl := regexp.MustCompile(`(?m)^\s{4}([a-zA-Z][a-zA-Z0-9]*)\s+(\[?\]?[a-zA-Z][a-zA-Z0-9]*)`)
	out := map[string]string{}
	for _, m := range decl.FindAllStringSubmatch(block[1], -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("%s: %q declares no arguments -- see above", path, mutation)
	}
	return out
}

// rendersAs answers what a Go value BECOMES once the step renderer has written
// it into MemQL source: the declared types that literal can then satisfy.
//
// `object` is deliberately absent from the map/struct answer's list only where
// the DSL has a narrower name for it; every arg below is checked against the
// type its own mutation declares, so an unmodelled pairing fails loudly rather
// than passing by omission.
func rendersAs(v any) []string {
	switch v.(type) {
	case string:
		return []string{"string"}
	case bool:
		return []string{"boolean", "bool"}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return []string{"int", "integer", "number", "float", "any"}
	case float32, float64:
		return []string{"number", "float", "any"}
	case []string:
		return []string{"[]string", "array", "any"}
	case []any:
		return []string{"array", "any"}
	case map[string]any:
		return []string{"object", "any"}
	}
	return nil
}

// The mutation each startup-payload group is forwarded to
// (dsl/cluster/automations.memql, steps databaseRecord and idpRecord).
const (
	clusterMutationsPath = "cluster/mutations.memql"
	databaseMutation     = "createDatabase"
	idpMutation          = "createIdentityProvider"
)

func TestStartupPayloadValuesSatisfyTheirMutationArgTypes(t *testing.T) {
	t.Setenv("MEMQL_DATABASE_DSN", "postgres://fixture:pw@db.invalid:15432/parsefixture?sslmode=require")
	t.Setenv("MEMQL_IDENTITY_VERIFIER_BASE_URL", "https://identity.example.com")
	t.Setenv("MEMQL_IDENTITY_VERIFIER_AUDIENCE", "memql")
	t.Setenv("MEMQL_IDENTITY_VERIFIER_JWKS_URL", "https://identity.example.com/.well-known/jwks.json")

	groups := []struct {
		what     string
		payload  map[string]any
		mutation string
	}{
		{"database", parseDatabaseInfo(), databaseMutation},
		{"identityProvider", parseIdentityProviderInfo(), idpMutation},
	}

	for _, g := range groups {
		if len(g.payload) == 0 {
			t.Fatalf("%s: the payload builder returned nothing for a DSN/base URL that names one -- "+
				"this gate cannot examine an empty map", g.what)
		}
		declared := declaredArgTypes(t, clusterMutationsPath, g.mutation)

		for name, value := range g.payload {
			want, ok := declared[name]
			if !ok {
				// Not a defect: the payload carries facts no mutation takes
				// (`engine`, `providerType` are stamped by the mutation body).
				continue
			}
			got := rendersAs(value)
			if got == nil {
				t.Errorf("%s.%s: a %T on the startup payload has no modelled rendering; "+
					"add it to rendersAs and say what MemQL literal it becomes",
					g.what, name, value)
				continue
			}
			if !contains(got, want) {
				t.Errorf("%s.%s is a Go %s, which renders as a MemQL %s literal, "+
					"but %s declares %q.\n"+
					"The step renders resolved args into MemQL source before the engine sees them, "+
					"so this is refused at argument validation on every start and no `??` default "+
					"can rescue it (memql#4926).",
					g.what, name, reflect.TypeOf(value), strings.Join(got, "/"), g.mutation, want)
			}
		}
	}
}

// The direct regression: a DSN's port reaches the payload as a NUMBER.
// The DSNs below name `parsefixture` rather than this project's shared test
// database, and nothing here connects to anything: `parseDatabaseInfo` only
// PARSES, and what these vary is the port, which is the whole subject. Naming
// the shared database in a fixture that never dials it is the confusion
// TestNoHardcodedSharedDSNInTests exists to prevent -- and routing through
// dbtest.DSN() would supply one port, which is the one thing these cannot use.
func TestDatabasePortFromDSNIsANumber(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want any
	}{
		{"the default port, named", "postgres://u:p@h:5432/parsefixture", 5432},
		{"a port that is not the default", "postgres://u:p@h:15432/parsefixture", 15432},
		{"no port at all", "postgres://u:p@h/parsefixture", 5432},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MEMQL_DATABASE_DSN", c.dsn)
			info := parseDatabaseInfo()
			if info == nil {
				t.Fatalf("parseDatabaseInfo returned nil for %q", c.dsn)
			}
			port, ok := info["port"]
			if !ok {
				t.Fatalf("no port on the payload for %q", c.dsn)
			}
			if port != c.want {
				t.Fatalf("port = %#v, want %#v -- a string here is refused by createDatabase's "+
					"`port int` on every bff start (memql#4926)", port, c.want)
			}
		})
	}
}

// A port `net/url` accepts but that is no port at all answers with NOTHING,
// rather than with a fabricated 5432: an absent key leaves the mutation to
// stamp its own default, and the payload never claims a reading nobody took.
func TestAnUnreadablePortIsOmittedRatherThanGuessed(t *testing.T) {
	t.Setenv("MEMQL_DATABASE_DSN", "postgres://u:p@h:99999999999999999999/parsefixture")
	info := parseDatabaseInfo()
	if info == nil {
		t.Skip("net/url refused the DSN outright, which is a stricter answer to the same question")
	}
	if v, ok := info["port"]; ok {
		t.Fatalf("port = %#v; want the key absent", v)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
