package cassette

import (
	"strings"
	"testing"
	"testing/fstest"
)

func tape() Cassette {
	return Cassette{
		Scenario: "durability.kill", Arm: "platform", ModelId: "synthetic", RecordedAt: "2026-09-06",
		Turns: []Turn{{
			RequestHash:  RequestHash("synthetic", "step=plan attempt=1"),
			Prompt:       "step=plan attempt=1",
			Response:     "a plan",
			InputTokens:  100,
			OutputTokens: 20,
			CostUSD:      0.001,
		}},
	}
}

func TestReadsIsTheCITiersHonestStandInForProviderCalls(t *testing.T) {
	p := NewPlayer(tape())
	if p.Reads() != 0 {
		t.Fatal("a fresh player has already read something")
	}
	if _, err := p.Serve("synthetic", "step=plan attempt=1"); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if p.Reads() != 1 {
		t.Fatalf("Reads = %d, want 1", p.Reads())
	}
}

func TestAMissIsAnErrorAndNotASilentEmptyAnswer(t *testing.T) {
	// The CI tier has no provider, so a miss means the scenario changed under
	// its cassette. Answering "" would let the run continue and report a
	// structural figure about a conversation that never happened.
	p := NewPlayer(tape())
	_, err := p.Serve("synthetic", "step=plan attempt=2")
	if err == nil {
		t.Fatal("Serve answered for a prompt the tape does not hold")
	}
	for _, want := range []string{"no recorded response", "memql-bench record", "2026-09-06", "synthetic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q, so the reader cannot tell what to do: %v", want, err)
		}
	}
	if p.Reads() != 0 {
		t.Error("a miss was counted as a read")
	}
	if len(p.Misses()) != 1 {
		t.Errorf("Misses = %v", p.Misses())
	}
}

func TestTheHashCoversTheMODELSoAModelChangeIsAMiss(t *testing.T) {
	// Serving the old reply for a new model would make a model upgrade
	// invisible to the whole suite.
	p := NewPlayer(tape())
	if _, err := p.Serve("claude-sonnet-5", "step=plan attempt=1"); err == nil {
		t.Fatal("the same prompt against a DIFFERENT model was served from the old recording")
	}
}

func TestTheRecordedCostIsNamedSoItCannotBeMistakenForAMeasurement(t *testing.T) {
	// Design P1: the CI tier publishes only what a replay can honestly
	// measure. These accessors exist so a live tier can compare shapes, and
	// their names are the last thing standing between them and a scorecard
	// column claiming CI measured dollars.
	p := NewPlayer(tape())
	if _, err := p.Serve("synthetic", "step=plan attempt=1"); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	in, out := p.RecordedTokens()
	if in != 100 || out != 20 {
		t.Fatalf("RecordedTokens = %d/%d", in, out)
	}
	if p.RecordedCost() != 0.001 {
		t.Fatalf("RecordedCost = %v", p.RecordedCost())
	}
}

func mapFS(name, body string) fstest.MapFS { return fstest.MapFS{"c/" + name: {Data: []byte(body)}} }

const goodTape = `{"scenario":"d.k","arm":"platform","modelId":"synthetic","recordedAt":"2026-09-06","turns":[]}`

func TestLoadInsistsTheNameAndTheContentAgree(t *testing.T) {
	// A cassette whose name and content disagree is one that can be served
	// for the wrong arm.
	_, err := Load(mapFS("d.k.baseline.json", goodTape), "c")
	if err == nil || !strings.Contains(err.Error(), "must agree") {
		t.Fatalf("Load = %v, want a refusal", err)
	}
	if _, err := Load(mapFS("d.k.platform.json", goodTape), "c"); err != nil {
		t.Fatalf("a well-named cassette was refused: %v", err)
	}
}

func TestACassetteMustSayWhatItRecordedAndWhen(t *testing.T) {
	// A recording read back months later is still a recording, and the
	// provenance is what stops it being read as today's measurement.
	noModel := `{"scenario":"d.k","arm":"platform","modelId":"","recordedAt":"2026-09-06","turns":[]}`
	if _, err := Load(mapFS("d.k.platform.json", noModel), "c"); err == nil || !strings.Contains(err.Error(), "still a recording") {
		t.Fatalf("Load = %v, want a refusal", err)
	}
}

func TestLoadRefusesAnUnknownField(t *testing.T) {
	bad := strings.Replace(goodTape, `"turns"`, `"trns"`, 1)
	if _, err := Load(mapFS("d.k.platform.json", bad), "c"); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}

func TestModelIdsSaysWhatWasOnTheTape(t *testing.T) {
	s, err := Load(mapFS("d.k.platform.json", goodTape), "c")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := s.ModelIds()
	if len(got) != 1 || got[0] != "synthetic" {
		t.Fatalf("ModelIds = %v", got)
	}
}
