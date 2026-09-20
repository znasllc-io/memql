package airoute

import (
	"os/exec"
	"strings"
	"testing"
)

func TestParseLevel_ClosedSet(t *testing.T) {
	for _, ok := range []string{"fast", "strong", "reasoning", "embeddings"} {
		if _, err := ParseLevel(ok); err != nil {
			t.Fatalf("ParseLevel(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "smart", "Fast", "cheap", "fast ", "embedding"} {
		err := mustRefuse(t, bad)
		// The message must name all four: it is what a DSL author and an
		// operator both read, and "unknown level" alone sends them to the
		// source to find out what is legal.
		for _, want := range []string{"fast", "strong", "reasoning", "embeddings"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("ParseLevel(%q) error %q does not name %q", bad, err, want)
			}
		}
	}
}

func mustRefuse(t *testing.T, s string) error {
	t.Helper()
	_, err := ParseLevel(s)
	if err == nil {
		t.Fatalf("ParseLevel(%q) was accepted; the set is closed", s)
	}
	return err
}

func TestLevelDegrade_ReasoningWalksDownAndEmbeddingsDoesNot(t *testing.T) {
	if next, ok := LevelReasoning.Degrade(); !ok || next != LevelStrong {
		t.Fatalf("reasoning degraded to %q (ok=%v); want strong", next, ok)
	}
	if next, ok := LevelStrong.Degrade(); !ok || next != LevelFast {
		t.Fatalf("strong degraded to %q (ok=%v); want fast", next, ok)
	}
	if _, ok := LevelFast.Degrade(); ok {
		t.Fatal("fast degraded; it is the floor")
	}
	// A degraded embedder answers in a DIFFERENT vector space, so the result
	// is not a worse vector -- it is one that does not belong in the index it
	// is about to be written to.
	if _, ok := LevelEmbeddings.Degrade(); ok {
		t.Fatal("embeddings degraded; a degraded embedder is a different vector space")
	}
}

func TestLevelsAndNames_AgreeAndCoverTheSet(t *testing.T) {
	if got := len(Levels()); got != 4 {
		t.Fatalf("Levels() has %d entries; the set is closed at four", got)
	}
	for _, l := range Levels() {
		if !l.Valid() {
			t.Fatalf("Levels() offers %q which ParseLevel refuses", l)
		}
		if !strings.Contains(LevelNames(), string(l)) {
			t.Fatalf("LevelNames() %q omits %q", LevelNames(), l)
		}
	}
}

func TestModality_ClosedSet(t *testing.T) {
	if got := len(Modalities()); got != 9 {
		t.Fatalf("Modalities() has %d entries; want the nine the seam serves", got)
	}
	for _, m := range Modalities() {
		if !m.Valid() {
			t.Fatalf("Modalities() offers %q which Valid refuses", m)
		}
	}
	if Modality("smart").Valid() {
		t.Fatal("an unknown modality validated")
	}
}

func TestEstimateMinContextTokens_IsNeverZero(t *testing.T) {
	if got := EstimateMinContextTokens("", 0); got <= 0 {
		t.Fatalf("estimate on an empty prompt is %d; it must never be zero -- a zero "+
			"floor admits every entry and reads exactly like a floor that was measured "+
			"and cleared", got)
	}
	if got := EstimateMinContextTokensFor(0); got <= 0 {
		t.Fatalf("estimate over no parts is %d; it must never be zero", got)
	}
}

func TestEstimateMinContextTokens_CountsPromptAndCompletion(t *testing.T) {
	// Four characters per token, rounded UP, plus the completion budget.
	if got, want := EstimateMinContextTokens(strings.Repeat("x", 401), 100), 101+100; got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
	if got, want := EstimateMinContextTokens(strings.Repeat("x", 400), 100), 100+100; got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
}

func TestEstimateMinContextTokensFor_SumsPartsWithoutConcatenating(t *testing.T) {
	parts := []string{strings.Repeat("a", 200), strings.Repeat("b", 201)}
	joined := EstimateMinContextTokens(parts[0]+parts[1], 50)
	if got := EstimateMinContextTokensFor(50, parts...); got != joined {
		t.Fatalf("summing parts gave %d, concatenating gave %d; they must agree", got, joined)
	}
}

func TestEstimateDefaultCompletionApplies(t *testing.T) {
	if got, want := EstimateMinContextTokens("", 0), 1+DefaultCompletionTokens; got != want {
		t.Fatalf("estimate with no declared completion budget = %d, want %d", got, want)
	}
}

func TestHasTag(t *testing.T) {
	r := ResolveRequest{Tags: []string{TagBackground}}
	if !r.HasTag(TagBackground) {
		t.Fatal("HasTag missed a tag the request carries")
	}
	if r.HasTag(TagBackgroundEscalation) {
		t.Fatal("HasTag claimed a tag the request does not carry")
	}
	if (ResolveRequest{}).HasTag(TagBackground) {
		t.Fatal("HasTag on a request with no tags answered true")
	}
}

// TestAirouteImportsOnlyTheStandardLibrary is the package's ONE guarantee that
// it stays nameable from every module in the workspace. The seam's halves are
// separate modules pointing one way, and every leaf that reaches a model --
// component/safety, component/fileprocessor, component/healing, component/work
// -- depends on core and on nothing else here. One repo-internal import would
// take that away silently: the package would still build, and the module edge
// would only surface as a red module-boundaries lane on somebody else's PR.
func TestAirouteImportsOnlyTheStandardLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	const self = "github.com/znasllc-io/memql/core/airoute"
	saw := false
	for _, dep := range strings.Fields(string(out)) {
		if dep == self {
			saw = true
			continue
		}
		if strings.HasPrefix(dep, "github.com/znasllc-io/memql") {
			t.Fatalf("core/airoute imports %s; it must stay a leaf so every module can name a level", dep)
		}
	}
	if !saw {
		t.Fatal("go list -deps did not report the package itself; the check proved nothing")
	}
}

// NeedsTools names the modalities on which MemQL DRIVES a tool loop. It is the
// predicate the app door turns on (design D7): on a tool turn MemQL is driving
// and an app is an agent that drives itself, so the door does not serve the
// turn -- it takes the whole STEP instead.
func TestModalityNeedsTools(t *testing.T) {
	want := map[Modality]bool{
		ModalityTools:          true,
		ModalityStreamingTools: true,
		ModalityChat:           false,
		ModalityStreamingChat:  false,
		ModalityStructured:     false,
		ModalityVision:         false,
		ModalityEmbedding:      false,
		ModalitySpeech:         false,
		ModalityTranscribe:     false,
	}
	for _, m := range Modalities() {
		if got := m.NeedsTools(); got != want[m] {
			t.Fatalf("%s.NeedsTools() = %v, want %v", m, got, want[m])
		}
	}
}

// EMBEDDINGS NEVER GO THROUGH AN APP (design D10), and the session door does
// not quietly become the exception: an embedding is not a tool turn, so an app
// entry is passed over for it exactly as before.
func TestEmbeddingsAreNeverToolNeeding(t *testing.T) {
	if ModalityEmbedding.NeedsTools() {
		t.Fatalf("embeddings must never be a tool-needing modality: a degraded or substituted embedder answers in a different vector space")
	}
}

// The four doors are distinct strings. `session` is a door rather than a flag
// on `app` because v1:router:call.door is what a reader filters on, and "an app
// answered a chat turn" and "an app ran the whole step" are different answers.
func TestDoorsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range []string{DoorLocal, DoorApp, DoorFederation, DoorSession} {
		if d == "" || seen[d] {
			t.Fatalf("door %q is empty or repeated", d)
		}
		seen[d] = true
	}
}
