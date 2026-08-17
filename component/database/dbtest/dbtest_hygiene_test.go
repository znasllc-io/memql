package dbtest

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// #2680: a wrong DSN pings identically to an absent database, so the
// skip message must name what it tried (password redacted), and
// MEMQL_REQUIRE_DB must convert the skip into a failure.
func TestSafeDSNRedactsPassword(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable": "postgres://memql:***@localhost:5432/memql?sslmode=disable",
		"postgres://memql@localhost:5432/memql":                           "postgres://memql@localhost:5432/memql",
		"postgres://localhost:5432/memql":                                 "postgres://localhost:5432/memql",
		"":                                                                "",
	} {
		if got := SafeDSN(in); got != want {
			t.Errorf("SafeDSN(%q) = %q, want %q", in, got, want)
		}
	}
	// The password must never survive redaction, whatever the shape.
	if strings.Contains(SafeDSN("postgres://u:sup3rs3cret@h:5432/d"), "sup3rs3cret") {
		t.Error("SafeDSN leaked the password")
	}
}

// memql#4032: "the port is open" is not evidence a database is there, and the
// two unreachable states call for opposite next steps. The k3d load balancer
// publishes 5432 and then EOFs partway through the Postgres handshake, so a
// developer with the cluster up has a connection that neither succeeds nor
// refuses -- and every db-gated case silently skips while the run reports ok.
// The message has to tell those two apart.
func TestReachabilityHintDistinguishesOpenPortFromNoListener(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // a phrase that must appear; "" means no hint at all
	}{
		{"nothing listening", fmt.Errorf("dial tcp [::1]:15999: connect: connection refused"), "nothing is listening"},
		{"k3d lb answers and EOFs", fmt.Errorf("EOF"), "the port is OPEN"},
		{"wrapped EOF", fmt.Errorf("ping: %w", fmt.Errorf("unexpected EOF")), "the port is OPEN"},
		{"peer reset", fmt.Errorf("read tcp: connection reset by peer"), "the port is OPEN"},
		{"authentication failure is neither", fmt.Errorf(`password authentication failed for user "memql"`), ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reachabilityHint(tc.err)
			if tc.want == "" {
				if got != "" {
					t.Errorf("reachabilityHint(%v) = %q, want no hint -- guessing at a failure "+
						"shape it cannot actually identify is worse than staying quiet", tc.err, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("reachabilityHint(%v) = %q, want it to contain %q", tc.err, got, tc.want)
			}
		})
	}

	// The two hints must not be interchangeable: a developer who reads the
	// wrong one goes and starts a database that is already running, or tears
	// down a cluster that was never the problem.
	refused := reachabilityHint(fmt.Errorf("connect: connection refused"))
	eof := reachabilityHint(fmt.Errorf("EOF"))
	if refused == eof {
		t.Fatal("the two unreachable states produce the same message, so the distinction memql#4032 asked for is not being made")
	}
}

func TestRequireDBParsing(t *testing.T) {
	prev, had := os.LookupEnv(RequireDBEnv)
	t.Cleanup(func() {
		if had {
			os.Setenv(RequireDBEnv, prev)
		} else {
			os.Unsetenv(RequireDBEnv)
		}
	})
	for value, want := range map[string]bool{
		"": false, "0": false, "false": false, "no": false,
		"1": true, "true": true, "yes": true, "TRUE": true,
	} {
		os.Setenv(RequireDBEnv, value)
		if got := RequireDB(); got != want {
			t.Errorf("%s=%q -> RequireDB()=%v, want %v", RequireDBEnv, value, got, want)
		}
	}
	os.Unsetenv(RequireDBEnv)
	if RequireDB() {
		t.Error("unset must not require a DB")
	}
}

// Unreachable must SKIP by default and FAIL under the opt-in. The
// failure branch is asserted through a recording TB so the test itself
// does not abort.
func TestUnreachableSkipsOrFails(t *testing.T) {
	prev, had := os.LookupEnv(RequireDBEnv)
	t.Cleanup(func() {
		if had {
			os.Setenv(RequireDBEnv, prev)
		} else {
			os.Unsetenv(RequireDBEnv)
		}
	})

	os.Setenv(RequireDBEnv, "1")
	rec := &recordingTB{}
	Unreachable(rec, "probe suite", "postgres://u:pw@h:5432/d", errProbe{})
	if !rec.failed {
		t.Error("MEMQL_REQUIRE_DB=1 must FAIL rather than skip")
	}
	if strings.Contains(rec.msg, "pw") {
		t.Errorf("failure message leaked the password: %s", rec.msg)
	}
	if !strings.Contains(rec.msg, "probe suite") || !strings.Contains(rec.msg, "UNREACHABLE") {
		t.Errorf("failure message must name the suite and the condition: %s", rec.msg)
	}

	os.Unsetenv(RequireDBEnv)
	rec = &recordingTB{}
	Unreachable(rec, "probe suite", "postgres://u:pw@h:5432/d", errProbe{})
	if !rec.skipped {
		t.Error("default must skip")
	}
	if strings.Contains(rec.msg, "pw") {
		t.Errorf("skip message leaked the password: %s", rec.msg)
	}
	if !strings.Contains(rec.msg, "postgres://u:***@h:5432/d") {
		t.Errorf("skip message must name the redacted DSN it tried: %s", rec.msg)
	}
	if !strings.Contains(rec.msg, RequireDBEnv) {
		t.Errorf("skip message must point at the opt-in: %s", rec.msg)
	}
}

type errProbe struct{}

func (errProbe) Error() string { return "SASL: password authentication failed" }

// recordingTB captures the Skipf/Fatalf a helper would issue.
type recordingTB struct {
	testing.TB
	failed  bool
	skipped bool
	msg     string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}
func (r *recordingTB) Skipf(format string, args ...any) {
	r.skipped = true
	r.msg = fmt.Sprintf(format, args...)
}
