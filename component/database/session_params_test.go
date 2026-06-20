package database

import (
	"strings"
	"testing"
)

// TestSessionConnParamsDefaults asserts the per-connection session safety net
// (memql#1817): both idle_in_transaction_session_timeout (wedged-mid-txn) and
// idle_session_timeout (orphan reaping) are on by default, the latter set above
// the client-side idle reaping so it only bites a dead pod's orphaned backend.
func TestSessionConnParamsDefaults(t *testing.T) {
	t.Setenv(envDBIdleInTxTimeoutMs, "")
	t.Setenv(envDBIdleSessionTimeoutMs, "")

	params := sessionConnParams()

	if got := params["idle_in_transaction_session_timeout"]; got != "60000" {
		t.Fatalf("idle_in_transaction_session_timeout = %v, want 60000", got)
	}
	if got := params["idle_session_timeout"]; got != "300000" {
		t.Fatalf("idle_session_timeout = %v, want 300000", got)
	}
}

func TestSessionConnParamsOverrides(t *testing.T) {
	t.Setenv(envDBIdleInTxTimeoutMs, "30000")
	t.Setenv(envDBIdleSessionTimeoutMs, "300000")

	params := sessionConnParams()

	if params["idle_in_transaction_session_timeout"] != "30000" {
		t.Fatalf("idle_in_transaction_session_timeout = %v, want 30000", params["idle_in_transaction_session_timeout"])
	}
	if params["idle_session_timeout"] != "300000" {
		t.Fatalf("idle_session_timeout = %v, want 300000", params["idle_session_timeout"])
	}
}

// TestSessionConnParamsExplicitZeroDisables confirms an explicit 0 omits the
// parameter so the server default stands (not "set to 0").
func TestSessionConnParamsExplicitZeroDisables(t *testing.T) {
	t.Setenv(envDBIdleInTxTimeoutMs, "0")

	if _, ok := sessionConnParams()["idle_in_transaction_session_timeout"]; ok {
		t.Fatalf("explicit 0 should omit idle_in_transaction_session_timeout")
	}
}

func TestDBApplicationName(t *testing.T) {
	t.Setenv(envDBAppName, "custom-name")
	if got := dbApplicationName(); got != "custom-name" {
		t.Fatalf("dbApplicationName() = %q, want custom-name", got)
	}

	t.Setenv(envDBAppName, "")
	t.Setenv("MEMQL_NODE_TYPE", "cognition")
	if got := dbApplicationName(); !strings.HasPrefix(got, "memql-cognition") {
		t.Fatalf("dbApplicationName() = %q, want memql-cognition prefix", got)
	}
}

func TestEnvIntDefault(t *testing.T) {
	t.Setenv("MEMQL_TEST_INT", "")
	if got := envIntDefault("MEMQL_TEST_INT", 7); got != 7 {
		t.Fatalf("blank should return default: got %d", got)
	}

	t.Setenv("MEMQL_TEST_INT", "notanumber")
	if got := envIntDefault("MEMQL_TEST_INT", 7); got != 7 {
		t.Fatalf("unparseable should return default: got %d", got)
	}

	t.Setenv("MEMQL_TEST_INT", "42")
	if got := envIntDefault("MEMQL_TEST_INT", 7); got != 42 {
		t.Fatalf("valid should parse: got %d", got)
	}
}
