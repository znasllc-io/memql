package automations

// loader_resolver_test.go -- regression coverage for memql#1663: run_automation
// name-resolution was inconsistent and many logic constructs were unreachable.
//
// LoadByName is now the single canonical resolver shared by the live AND
// dry-run run_automation paths. It resolves:
//   - the exact automation name (the canonical entry point, always supported),
//   - a wrapped logic construct by its full `logicXxx` name, and
//   - a wrapped logic construct by its bare invocation form `xxx`,
// and returns a clear "not a runnable entry point" error for a logic construct
// that no automation wraps (internal-only) -- instead of a misleading "not
// found".

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func newResolverLoader(t *testing.T) *Loader {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewLoader(LoaderOptions{Logger: logger, Registry: nil})
}

// TestLoadByName_CanonicalResolution proves the deterministic canonical-name
// rule on the live path.
func TestLoadByName_CanonicalResolution(t *testing.T) {
	loader := newResolverLoader(t)

	cases := []struct {
		name     string // input passed to LoadByName
		wantAuto string // expected resolved automation name
		wantErr  string // substring expected in the error (empty == expect success)
	}{
		// Exact automation names -- the canonical entry points. These are the
		// forms that already worked and MUST keep working (#1643 cluster logics).
		{name: "registerNode", wantAuto: "registerNode"},
		{name: "deregisterNode", wantAuto: "deregisterNode"},
		{name: "bootstrapCluster", wantAuto: "bootstrapCluster"},
		{name: "pruneStaleClusterNodes", wantAuto: "pruneStaleClusterNodes"},
		{name: "expireDelegations", wantAuto: "expireDelegations"},

		// Full `logicXxx` construct name -> wrapping automation. Previously
		// failed with "automation not found".
		{name: "logicRegisterNode", wantAuto: "registerNode"},
		{name: "logicRevokeExpiredDelegations", wantAuto: "expireDelegations"},

		// Bare invocation form -> wrapping automation. This is the
		// non-mechanical case called out in #1663: the bare name
		// `revokeExpiredDelegations` does NOT match the automation name
		// `expireDelegations`, yet it must resolve.
		{name: "revokeExpiredDelegations", wantAuto: "expireDelegations"},

		// A logic construct that exists but no automation wraps -> a clear
		// not-a-runnable-entry-point error, NOT a misleading "not found".
		{name: "logicEnsureDailySpaceForCaller", wantErr: "not a runnable entry point"},
		{name: "ensureDailySpaceForCaller", wantErr: "not a runnable entry point"},

		// Genuinely unknown name -> plain "not found" (and NOT the
		// not-a-runnable-entry-point error).
		{name: "totallyBogusConstructXyz", wantErr: "not found"},

		// Empty name -> required error.
		{name: "   ", wantErr: "required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auto, err := loader.LoadByName(tc.name)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadByName(%q): expected error containing %q, got nil (auto=%v)", tc.name, tc.wantErr, auto)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadByName(%q): expected error containing %q, got %q", tc.name, tc.wantErr, err.Error())
				}
				// The not-found case must NOT masquerade as not-a-runnable-entry-point.
				if tc.wantErr == "not found" && strings.Contains(err.Error(), "not a runnable entry point") {
					t.Fatalf("LoadByName(%q): unknown name leaked the entry-point error: %q", tc.name, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadByName(%q): unexpected error: %v", tc.name, err)
			}
			if auto == nil {
				t.Fatalf("LoadByName(%q): resolved to nil automation", tc.name)
			}
			if auto.Name != tc.wantAuto {
				t.Fatalf("LoadByName(%q): resolved to automation %q, want %q", tc.name, auto.Name, tc.wantAuto)
			}
		})
	}
}

// TestLoadByName_LiveAndDryRunResolveSameConstruct proves the live and dry-run
// run_automation paths resolve the SAME construct with NO divergence: the live
// path resolves via LoadByName, then the dry-run path fetches that resolved
// automation's source via DSLConstructSource(kind="automation", canonicalName).
// Before #1663 the dry-run source lookup returned false (kind hardcoded against
// an extractor that never emitted automation slices), so the two paths diverged.
func TestLoadByName_LiveAndDryRunResolveSameConstruct(t *testing.T) {
	loader := newResolverLoader(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Resolve by a wrapped logic name -- the form that previously broke -- on
	// both paths and assert they converge on the same automation + a real source.
	for _, input := range []string{"logicRevokeExpiredDelegations", "revokeExpiredDelegations", "expireDelegations"} {
		t.Run(input, func(t *testing.T) {
			// Live resolution.
			auto, err := loader.LoadByName(input)
			if err != nil {
				t.Fatalf("live LoadByName(%q): %v", input, err)
			}
			if auto.Name != "expireDelegations" {
				t.Fatalf("live LoadByName(%q): resolved %q, want expireDelegations", input, auto.Name)
			}

			// Dry-run source fetch by the resolved canonical name (exactly what
			// the runner's dry-run path does).
			src, ok := memql.DSLConstructSource(logger, "automation", auto.Name)
			if !ok {
				t.Fatalf("dry-run DSLConstructSource(automation, %q): not found (the #1663 gap)", auto.Name)
			}
			if !strings.Contains(src, "automation "+auto.Name) {
				t.Fatalf("dry-run source for %q does not contain its declaration:\n%s", auto.Name, src)
			}
		})
	}
}

func TestLogicNameHelpers(t *testing.T) {
	cases := []struct {
		in          string
		isConstruct bool
		full        string
		bare        string
	}{
		{in: "logicRegisterNode", isConstruct: true, full: "logicRegisterNode", bare: "registerNode"},
		{in: "registerNode", isConstruct: false, full: "logicRegisterNode", bare: "registerNode"},
		{in: "logicRevokeExpiredDelegations", isConstruct: true, full: "logicRevokeExpiredDelegations", bare: "revokeExpiredDelegations"},
		// "logical" is an ordinary identifier, not a `logicXxx` construct (the
		// char after the prefix is lowercase), so the helpers leave it alone.
		{in: "logical", isConstruct: false, full: "logicLogical", bare: "logical"},
	}
	for _, tc := range cases {
		if got := isLogicConstructName(tc.in); got != tc.isConstruct {
			t.Errorf("isLogicConstructName(%q) = %v, want %v", tc.in, got, tc.isConstruct)
		}
		if got := fullLogicName(tc.in); got != tc.full {
			t.Errorf("fullLogicName(%q) = %q, want %q", tc.in, got, tc.full)
		}
		if got := bareLogicName(tc.in); got != tc.bare {
			t.Errorf("bareLogicName(%q) = %q, want %q", tc.in, got, tc.bare)
		}
	}
}
