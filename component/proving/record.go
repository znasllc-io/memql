package proving

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/znasllc-io/memql/component/proving/cassette"
	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
)

// SyntheticModelId marks a cassette whose responses are PLACEHOLDERS rather
// than a recording of a real model.
//
// This is the honesty seam of the whole CI tier, so it is worth being blunt
// about what a synthetic cassette is and is not.
//
// A cassette's job is to make a run deterministic without a provider. Its
// CONTENT -- the words a model said -- affects no structural measurement: how
// many model exchanges a plan needs, how many steps a resume re-executes, and
// whether an effect duplicated are properties of the PLAN and the PLATFORM,
// not of the model's prose. So a placeholder response measures those honestly.
//
// What it CANNOT measure is anything model-dependent: tokens, dollars, whether
// the model's answer was any good. Design P1 already forbids the CI tier from
// publishing those, and Figures() emits `notMeasurableOnReplay` for every one
// of them. The sentinel is the belt to that braces: a figure derived from a
// synthetic cassette carries "synthetic" as its model id, so a number that
// escaped into a model-dependent column would say so on the page.
//
// `memql-bench record` against a real provider replaces a synthetic cassette
// with a real recording, and nothing else changes.
const SyntheticModelId = "synthetic"

// maxRecordedAttempts is how many attempts of each reasoning step a synthetic
// cassette covers. The baseline arm restarts up to three times and the
// platform arm resumes once, so five is comfortably above what any scenario in
// the corpus can reach -- and a miss is a loud error rather than a silent
// empty answer, so an under-generated cassette fails visibly.
const maxRecordedAttempts = 8

// SyntheticCassette builds a placeholder cassette covering every reasoning
// step a scenario can reach on one arm.
func SyntheticCassette(s scenario.Scenario, arm figure.Arm, recordedAt string) cassette.Cassette {
	c := cassette.Cassette{
		Scenario:   s.Id,
		Arm:        string(arm),
		ModelId:    SyntheticModelId,
		RecordedAt: recordedAt,
	}
	for _, st := range s.Steps {
		if !st.Reasoning {
			continue
		}
		for attempt := 1; attempt <= maxRecordedAttempts; attempt++ {
			prompt := promptFor(st, attempt)
			c.Turns = append(c.Turns, cassette.Turn{
				RequestHash: cassette.RequestHash(SyntheticModelId, prompt),
				Prompt:      prompt,
				Response:    fmt.Sprintf("placeholder answer for %s (attempt %d)", st.Key, attempt),
				// Zero tokens and zero cost, deliberately. A synthetic
				// recording has no cost, and writing a plausible-looking one
				// would put an invented number where a measured one belongs.
			})
		}
	}
	return c
}

// WriteCassette writes one cassette to dir under its canonical name.
func WriteCassette(dir string, c cassette.Cassette) (string, error) {
	name := fmt.Sprintf("%s.%s.json", c.Scenario, c.Arm)
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("proving: marshalling %s: %w", name, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("proving: writing %s: %w", path, err)
	}
	return path, nil
}

// NeedsCassette reports whether a scenario has any reasoning step, and
// therefore needs a cassette on each arm.
func NeedsCassette(s scenario.Scenario) bool { return needsModel(s) }
