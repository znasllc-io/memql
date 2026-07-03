package memql

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// TestLoadReport_HasProblems is the strict-boot decision predicate (epic
// #2351 / S2, memql#2357): a skipped in-tree construct OR a same-registry
// duplicate is a boot-blocking problem; a re-hydration quarantine is NOT
// (stored authored rows can rot and must not brick a fleet).
func TestLoadReport_HasProblems(t *testing.T) {
	t.Run("empty is clean", func(t *testing.T) {
		r := newLoadReport()
		if r.HasProblems() {
			t.Fatal("empty report should have no problems")
		}
		if r.ProblemCount() != 0 {
			t.Fatalf("empty report ProblemCount = %d, want 0", r.ProblemCount())
		}
	})

	t.Run("skip is a problem", func(t *testing.T) {
		r := newLoadReport()
		r.AddSkip(baseloader.Skip{Component: "x", Keyword: "shape", Name: "s", File: "f", Phase: "parse", Err: "boom"})
		if !r.HasProblems() {
			t.Fatal("a skip should be a strict-boot problem")
		}
		if r.ProblemCount() != 1 {
			t.Fatalf("ProblemCount = %d, want 1", r.ProblemCount())
		}
	})

	t.Run("duplicate is a problem", func(t *testing.T) {
		r := newLoadReport()
		r.SetDuplicates([]DuplicateConstruct{{Group: "shapes", Name: "dup", Origins: []string{"shape a", "shape b"}}})
		if !r.HasProblems() {
			t.Fatal("a duplicate should be a strict-boot problem")
		}
	})

	t.Run("quarantine alone is NOT a problem", func(t *testing.T) {
		r := newLoadReport()
		r.AddQuarantine(QuarantinedConstruct{Kind: "spec", Name: "rotted", Err: "grammar drift"})
		if r.HasProblems() {
			t.Fatal("a quarantine must NOT fail strict boot -- stored authored rows can rot")
		}
		if r.ProblemCount() != 0 {
			t.Fatalf("ProblemCount = %d, want 0 (quarantines excluded)", r.ProblemCount())
		}
	})
}

// TestLoadReport_FoldSink proves the baseloader-backed loaders correctly
// merge their skip sink + registered count into the report.
func TestLoadReport_FoldSink(t *testing.T) {
	r := newLoadReport()
	sink := newBaseloaderSink()
	sink.Add(baseloader.Skip{Component: "memql.unifiedShapeLoader", Keyword: "shape", Name: "bad", File: "x/shapes.memql", Phase: "parse", Err: "boom"})
	r.FoldSink("shapes", 7, sink)

	if got := r.Registered["shapes"]; got != 7 {
		t.Fatalf("registered[shapes] = %d, want 7", got)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Name != "bad" {
		t.Fatalf("expected 1 folded skip named 'bad', got %+v", r.Skipped)
	}
	if !strings.Contains(r.Detail(), "bad") {
		t.Fatalf("Detail() should name the skipped construct, got:\n%s", r.Detail())
	}
}

// TestDSLAllowSkips exercises the break-glass env parse. Truthy variants
// enable boot-despite-problems; unset/malformed keeps strict.
func TestDSLAllowSkips(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"nope":  false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"on":    true,
	}
	for v, want := range cases {
		t.Run("val="+v, func(t *testing.T) {
			t.Setenv(allowSkipsEnvVar, v)
			if got := dslAllowSkips(); got != want {
				t.Fatalf("dslAllowSkips() with %q = %v, want %v", v, got, want)
			}
		})
	}
}

// TestRehydrationQuarantine covers the durable-bundle re-hydration
// posture (item 3): a STORED authored construct whose source no longer
// recompiles is QUARANTINED -- the recompile returns an error (so the
// boot walk counts it), the engine records a quarantine entry + ERROR
// log, and crucially it does NOT become a strict-boot problem (boot
// continues). This is the deliberate asymmetry vs. in-tree constructs.
func TestRehydrationQuarantine(t *testing.T) {
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	eng.loadReport = newLoadReport()

	// A durably-promoted spec whose stored source is garbage under the
	// current grammar -- exactly the "row authored under an older grammar
	// no longer recompiles" rot case (#2361 completes the picture).
	row := AuthoringConstructRow{
		Kind:        "spec",
		Name:        "rottedSpec",
		BundleId:    "authoring:bundle:rot1",
		OwnerUserId: "u-owner",
		Source:      "spec activeRowTrait rottedSpec {\n  return status ==== \"x\" &&&& true\n}\n",
		Status:      "active",
	}

	rerr := eng.recompileAndPromoteRow(context.Background(), row)
	if rerr == nil {
		t.Fatal("recompileAndPromoteRow should return an error for a rotted stored spec")
	}
	if len(eng.loadReport.Quarantined) != 1 {
		t.Fatalf("expected exactly 1 quarantine entry, got %d: %+v", len(eng.loadReport.Quarantined), eng.loadReport.Quarantined)
	}
	q := eng.loadReport.Quarantined[0]
	if q.Name != "rottedSpec" || q.Kind != "spec" || q.BundleId != "authoring:bundle:rot1" || q.Owner != "u-owner" {
		t.Fatalf("quarantine entry mis-populated: %+v", q)
	}
	// The whole point: a rotted stored row must NOT fail strict boot.
	if eng.loadReport.HasProblems() {
		t.Fatal("a re-hydration quarantine must NOT register as a strict-boot problem")
	}
}
