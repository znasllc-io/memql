package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/proving/capability"
)

// dsnSpec is a minimal spec carrying the two params these tests use. The real
// one is built inside run(); reproducing it here would be a second copy that
// can disagree, and nothing below depends on the rest of it.
func dsnSpec() capability.Spec {
	return capability.Spec{
		Id: "bench.run",
		Params: []capability.Param{
			{Name: "do"},
			{Name: "dsn"},
		},
	}
}

// --dsn has to reach dbtest.EnsureSchema, which takes no argument and reads
// MEMQL_DATABASE_DSN itself.
//
// Before this was wired, --dsn migrated one database and queried another. The
// run did not fail there: it got an engine, ran scenarios, and died several
// steps later on `relation "MemoryNodes" does not exist` -- a Postgres error
// that names a table, says nothing about migrations, and points at neither
// database. Against a developer machine that had ever run a db-gated test the
// bug was invisible, because the default database already had the schema.
//
// The assertion is on the ENVIRONMENT rather than on a successful run,
// deliberately: it needs no database, and the environment is the actual
// channel between the flag and the helper.
func TestTheDsnFlagIsPublishedForTheMigrationStep(t *testing.T) {
	const flagDSN = "postgres://someone:secret@127.0.0.1:1/from_the_flag?sslmode=disable"
	// A DIFFERENT value in the environment, so a pass cannot come from the
	// two agreeing. The capability contract puts flags above the environment.
	t.Setenv(dsnEnv, "postgres://other:secret@127.0.0.1:1/from_the_env?sslmode=disable")

	c, handled, err := capability.Parse(dsnSpec(), []string{"--do=gate", "--dsn=" + flagDSN}, io.Discard, io.Discard)
	if err != nil || handled {
		t.Fatalf("parse: err=%v handled=%v", err, handled)
	}

	// 127.0.0.1:1 refuses immediately, so openEngine fails at the ping. That
	// is fine and expected -- the publish happens before it.
	eng, closer, code := openEngine(c)
	if eng != nil || code == capability.ExitOK {
		if closer != nil {
			closer()
		}
		t.Fatalf("expected openEngine to fail against an unreachable DSN, got code=%d", code)
	}

	if got := os.Getenv(dsnEnv); got != flagDSN {
		t.Errorf("MEMQL_DATABASE_DSN = %q, want the --dsn value %q\n"+
			"the migration step reads this variable, so a --dsn that does not reach it migrates a different database than the run queries", got, flagDSN)
	}
}

// The environment is used when --dsn is absent. Without this, the test above
// would pass against an openEngine that ignored the environment entirely and
// always published the flag -- including publishing an empty string over a
// perfectly good ambient DSN.
func TestTheEnvironmentIsUsedWhenNoDsnFlagIsGiven(t *testing.T) {
	const envDSN = "postgres://someone:secret@127.0.0.1:1/from_the_env?sslmode=disable"
	t.Setenv(dsnEnv, envDSN)

	c, _, err := capability.Parse(dsnSpec(), []string{"--do=gate"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, closer, code := openEngine(c); code == capability.ExitOK {
		if closer != nil {
			closer()
		}
		t.Fatal("expected failure against an unreachable DSN")
	}
	if got := os.Getenv(dsnEnv); got != envDSN {
		t.Errorf("MEMQL_DATABASE_DSN = %q, want it left as %q", got, envDSN)
	}
}

// A DSN carries a password, and openEngine's unreachable-database failure puts
// it in the JSON envelope on stdout -- which CI publishes into a job summary.
func TestRedactDSNRemovesThePassword(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		wantNot  string
		want     string
	}{
		{
			name:    "password is replaced",
			in:      "postgres://memql:hunter2@db.internal:5432/memql?sslmode=disable",
			wantNot: "hunter2",
			want:    "postgres://memql:xxxxx@db.internal:5432/memql",
		},
		{
			name: "no password, nothing invented",
			in:   "postgres://memql@db.internal:5432/memql",
			want: "postgres://memql@db.internal:5432/memql",
		},
		{
			name: "no user at all",
			in:   "postgres://db.internal:5432/memql",
			want: "postgres://db.internal:5432/memql",
		},
		{
			// A shape it cannot parse is the one whose password it would fail
			// to find, so it must not echo the input back.
			name:    "unparseable becomes a fixed string",
			in:      "host=db.internal user=memql password=hunter2",
			wantNot: "hunter2",
			want:    "the configured DSN",
		},
		{
			name:    "empty",
			in:      "",
			want:    "the configured DSN",
			wantNot: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.in)
			if got != tc.want {
				t.Errorf("redactDSN(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("redactDSN(%q) = %q, which still contains the secret %q", tc.in, got, tc.wantNot)
			}
		})
	}
}
