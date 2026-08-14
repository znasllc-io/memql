package database

// search_path_test.go -- the parse-and-refuse half of the environment boundary
// (memql#3765). The half that needs a real database is in
// search_path_db_test.go.

import (
	"database/sql/driver"
	"strings"
	"testing"
)

func TestParseSearchPathAcceptsTheDeploymentValue(t *testing.T) {
	got, err := parseSearchPath("memql_staging, public")
	if err != nil {
		t.Fatalf("parseSearchPath: %v", err)
	}
	if len(got) != 2 || got[0] != "memql_staging" || got[1] != "public" {
		t.Fatalf("parseSearchPath = %v, want [memql_staging public]", got)
	}
	if targetSchema(got) != "memql_staging" {
		t.Errorf("targetSchema = %q, want the FIRST element (that is where an unqualified CREATE lands)", targetSchema(got))
	}
}

// The value arrives from a k8s secret, so tolerate the spacing a human writes
// -- but only the spacing. Everything else is refused below.
func TestParseSearchPathToleratesSpacing(t *testing.T) {
	for _, raw := range []string{
		"memql_prod,public",
		"memql_prod , public",
		"  memql_prod,   public  ",
		"memql_prod,,public",
	} {
		got, err := parseSearchPath(raw)
		if err != nil {
			t.Errorf("parseSearchPath(%q): %v", raw, err)
			continue
		}
		if len(got) != 2 || got[0] != "memql_prod" || got[1] != "public" {
			t.Errorf("parseSearchPath(%q) = %v", raw, got)
		}
	}
}

// A search path is the environment boundary. A value the parser cannot make
// sense of means the operator meant something the engine cannot deliver, and
// dropping the unparseable element is how a node comes up pointed at the wrong
// schema while looking configured.
func TestParseSearchPathRefusesWhatItCannotDeliver(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"an injected statement", `memql_prod; DROP SCHEMA public`, "bare schema identifier"},
		{"a quoted identifier", `"Mixed Case"`, "bare schema identifier"},
		{"upper case", "MemQL_Prod", "bare schema identifier"},
		{"a leading digit", "9lives", "bare schema identifier"},
		{"nothing at all", "  ,  ", "names no schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSearchPath(tc.raw)
			if err == nil {
				t.Fatalf("parseSearchPath(%q) accepted a value it cannot safely interpolate", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// The whole safety margin is that an unset or mistyped path resolves to a
// schema holding nothing. An environment whose FIRST schema is `public` is
// exactly where the fallback points, so the mistyped case would stop failing
// and start silently meaning that environment.
func TestParseSearchPathRefusesPublicAsTheEnvironmentSchema(t *testing.T) {
	for _, raw := range []string{"public", "public, memql_prod"} {
		_, err := parseSearchPath(raw)
		if err == nil {
			t.Fatalf("parseSearchPath(%q) accepted public as the environment schema, which removes the safety margin while looking configured", raw)
		}
		if !strings.Contains(err.Error(), "public") {
			t.Errorf("error %q does not name public", err)
		}
	}
	// It is fine, and required, anywhere else on the path -- that is where the
	// extension functions live.
	if _, err := parseSearchPath("memql_prod, public"); err != nil {
		t.Errorf("public must still be allowed after the environment schema: %v", err)
	}
}

func TestSearchPathFromEnv(t *testing.T) {
	t.Setenv(envDBSearchPath, "  memql_staging, public  ")
	if got := searchPathFromEnv(); got != "memql_staging, public" {
		t.Errorf("searchPathFromEnv() = %q", got)
	}
	t.Setenv(envDBSearchPath, "")
	if got := searchPathFromEnv(); got != "" {
		t.Errorf("unset must read as empty (the single-environment case), got %q", got)
	}
}

// The statement is built once at wrap time and reused, so pin its shape: a
// LITERAL list, because a bound parameter would be one schema name (see the
// file header for the measurement).
func TestNewSearchPathConnectorBuildsALiteralList(t *testing.T) {
	base := &fakeConnector{}
	c, ok := newSearchPathConnector(base, []string{"memql_prod", "public"}).(*searchPathConnector)
	if !ok {
		t.Fatal("expected a searchPathConnector")
	}
	if c.stmt != "SET search_path = memql_prod, public" {
		t.Errorf("stmt = %q", c.stmt)
	}
}

// No configured path means the single-environment case, and that must compose
// to exactly the connector chain that existed before this file.
func TestNewSearchPathConnectorIsTransparentWhenUnconfigured(t *testing.T) {
	base := &fakeConnector{}
	if got := newSearchPathConnector(base, nil); got != driver.Connector(base) {
		t.Errorf("an empty schema list must return the base connector untouched, got %T", got)
	}
	if got := newSearchPathConnector(nil, []string{"memql_prod"}); got != nil {
		t.Errorf("a nil base must stay nil so it composes with newRetryingConnector, got %T", got)
	}
}
