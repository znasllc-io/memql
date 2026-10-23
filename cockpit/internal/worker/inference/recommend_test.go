package inference

import (
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
)

// hostMeetingTheFloor is a machine with nothing wrong with it, so a
// test about the recommended set is not also a test about a refusal.
func hostMeetingTheFloor() Host {
	return Host{
		GOOS:     "darwin",
		GOARCH:   "arm64",
		Floor:    metFloor("apple silicon, 64 GB, macOS 15"),
		LookPath: lookPath("ollama"),
	}
}

// The sets, BY NAME. The acceptance criterion for this change is that
// the default pair is asserted by name, so these are literals rather
// than references to the constants -- a test comparing two expressions
// that both read DefaultGeneralModel would pass on any pair at all.
func TestRecommendedSetByClass(t *testing.T) {
	for _, tc := range []struct {
		class string
		want  []string
	}{
		// ONE text model per class plus the cluster's embedder, and the
		// 24 GB rung is its own (2026-09-08 record, D5-D7). A single
		// text model leaves room for the embedder and working context.
		{"unsupported", []string{"qwen3.5:4b", "qwen3-embedding:0.6b"}},
		{"16", []string{"qwen3.5:9b", "qwen3-embedding:0.6b"}},
		// THE RUNG THAT WAS MISSING: a 24 GB card is its own set, not a
		// repeat of the 16 GB pair (2026-09-08 record, D5).
		{"24", []string{"qwen3.8:27b", "qwen3-embedding:0.6b"}},
		{"32", []string{"qwen3.8:27b", "qwen3-embedding:0.6b"}},
		{"64", []string{"qwen3.8:27b-q8_0", "qwen3-embedding:0.6b"}},
		{"128", []string{"qwen3.8:27b-q8_0", "qwen3-embedding:0.6b"}},
		{"", []string{"qwen3.5:4b", "qwen3-embedding:0.6b"}},
		{"nonsense", []string{"qwen3.5:4b", "qwen3-embedding:0.6b"}},
	} {
		t.Run(tc.class, func(t *testing.T) {
			got := RecommendedSet(tc.class)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("RecommendedSet(%q) =\n  %v\nwant\n  %v", tc.class, got, tc.want)
			}
		})
	}
}

// The August pair is GONE. Named explicitly, because the failure this
// guards is a constant creeping back in through a default somewhere --
// and a fleet quietly pulling llama3.1 again would look like nothing at
// all from here.
func TestTheAugustPairIsNoLongerRecommendedAnywhere(t *testing.T) {
	for _, class := range []string{"unsupported", "16", "24", "32", "64", "128", ""} {
		for _, id := range RecommendedSet(class) {
			if id == "llama3.1:8b" || id == "nomic-embed-text" {
				t.Fatalf("class %q still recommends the August default %q", class, id)
			}
		}
	}
}

// The embedder goes last on every class: a run interrupted halfway
// should leave a machine with a general model, not with only an
// embedder -- which answers no prompts while looking configured.
func TestRecommendedSetPutsTheEmbedderLast(t *testing.T) {
	for _, class := range []string{"unsupported", "16", "24", "32", "64", "128"} {
		set := RecommendedSet(class)
		if len(set) == 0 {
			t.Fatalf("class %q recommends nothing; every class gets a set", class)
		}
		if !strings.Contains(set[len(set)-1], "embedding") {
			t.Fatalf("class %q ends with %q, want the embedder last", class, set[len(set)-1])
		}
		for _, id := range set[:len(set)-1] {
			if strings.Contains(id, "embedding") {
				t.Fatalf("class %q has an embedder at a non-final position: %v", class, set)
			}
		}
	}
}

// Every class recommends exactly one embedder. Two would double the
// engine's embedding routing choice for no stated reason; none would
// leave half the platform's local operations unservable.
func TestRecommendedSetHasExactlyOneEmbedder(t *testing.T) {
	for _, class := range []string{"unsupported", "16", "24", "32", "64", "128"} {
		n := 0
		for _, id := range RecommendedSet(class) {
			if strings.Contains(id, "embedding") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("class %q recommends %d embedders, want 1", class, n)
		}
	}
}

// Decide reads the class off the Host and never probes for it, so the
// whole table is drivable from a fixture.
func TestDecidePutsTheClassAndItsSetOnThePlan(t *testing.T) {
	h := hostMeetingTheFloor()
	h.Hardware = hardware.Inventory{
		MemoryBytes: 64 << 30,
		GPU:         &hardware.GPU{Name: "Apple M3 Max", VRAMBytes: 64 << 30, Backend: hardware.BackendMetal},
	}
	p := Decide(h)
	if p.MachineClass != "32" {
		t.Fatalf("MachineClass = %q, want 32 (48 GB usable of 64 GB unified)", p.MachineClass)
	}
	want := []string{"qwen3.8:27b", "qwen3-embedding:0.6b"}
	if !reflect.DeepEqual(p.DefaultModels, want) {
		t.Fatalf("DefaultModels = %v, want %v", p.DefaultModels, want)
	}
}

// A REFUSAL still carries both. "This machine cannot serve models" and
// "the set it would have pulled is this" are both facts a person
// reading a refusal wants; blanking the second reads as a command that
// gave up before deciding anything.
func TestARefusalStillCarriesTheClassAndTheSet(t *testing.T) {
	p := Decide(Host{GOOS: "linux", GOARCH: "amd64"})
	if p.Refusal == "" {
		t.Fatal("want a refusal for a host with no floor verdict")
	}
	if p.MachineClass == "" {
		t.Fatal("a refusal blanked MachineClass")
	}
	if len(p.DefaultModels) == 0 {
		t.Fatal("a refusal blanked DefaultModels")
	}
}
