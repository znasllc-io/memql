package work

import (
	"errors"
	"strings"
	"testing"
)

func TestEveryOverrideRefusalNamesItsField(t *testing.T) {
	cases := []struct {
		name  string
		o     Override
		field string
		code  string
	}{
		{"embeddings is never a level", Override{Level: "embeddings"}, "level", OverrideLevelInvalid},
		{"unknown level", Override{Level: "genius"}, "level", OverrideLevelInvalid},
		{"unknown effort", Override{Effort: "extreme"}, "effort", OverrideEffortInvalid},
		{"prompt too long", Override{Prompt: strings.Repeat("x", maxOverridePromptBytes+1)}, "prompt", OverridePromptTooLong},
		{"model with a space", Override{Model: "app:claude code"}, "model", OverrideModelInvalid},
		{"model padded", Override{Model: " fleet:m "}, "model", OverrideModelInvalid},
		{"model too long", Override{Model: strings.Repeat("m", maxOverrideModelBytes+1)}, "model", OverrideModelInvalid},
		{"blank input name", Override{Inputs: map[string]any{" ": 1}}, "inputs", OverrideInputKeyInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var oe *OverrideError
			if err := ValidateOverride(c.o); !errors.As(err, &oe) || oe.Field != c.field || oe.Code != c.code {
				t.Fatalf("ValidateOverride(%+v) = %v, want %s on %s", c.o, err, c.code, c.field)
			}
		})
	}
}

func TestAValidOverridePasses(t *testing.T) {
	o := Override{Level: "reasoning", Model: "app:claude-code:opus", Effort: "max", Prompt: "Use the totals.", Inputs: map[string]any{"month": "2026-08"}}
	if err := ValidateOverride(o); err != nil {
		t.Fatalf("ValidateOverride = %v", err)
	}
}

func TestAnOverrideOfOnlyTheLevelIsNotAuthored(t *testing.T) {
	if (Override{Level: "strong", Model: "fleet:m", Effort: "high"}).Authored() {
		t.Error("changing only the intelligence is still the system's own input (D20)")
	}
	if !(Override{Prompt: "Do it this way."}).Authored() || !(Override{Inputs: map[string]any{"a": 1}}).Authored() {
		t.Error("a person who wrote the prompt or the inputs authored the version")
	}
}

func TestAnOverrideWithOnlyARequesterIsEmpty(t *testing.T) {
	if !(Override{RequestedBy: "u1"}).Empty() {
		t.Error("who asked is not a change")
	}
	if (Override{Guidance: &Guidance{Reason: "the totals are missing"}}).Empty() {
		t.Error("guidance is a change: it rides the next attempt")
	}
}

func TestTheOverrideRoundTripsThroughItsStoredForm(t *testing.T) {
	o := Override{
		Level: "strong", Model: "fleet:m", Effort: "high", Prompt: "Use the totals.",
		Inputs:      map[string]any{"month": "2026-08"},
		Guidance:    &Guidance{Axes: Axes{Product: true}, Reason: "the totals are missing", FeedbackId: "obs-1"},
		RequestedBy: "u1",
	}
	got := ParseOverride(o.Object())
	if got.Level != o.Level || got.Model != o.Model || got.Effort != o.Effort || got.Prompt != o.Prompt || got.RequestedBy != o.RequestedBy {
		t.Errorf("round trip = %+v, want %+v", got, o)
	}
	if got.Inputs["month"] != "2026-08" {
		t.Errorf("inputs = %v", got.Inputs)
	}
	if got.Guidance == nil || !got.Guidance.Axes.Product || got.Guidance.Reason != "the totals are missing" || got.Guidance.FeedbackId != "obs-1" {
		t.Errorf("guidance = %+v", got.Guidance)
	}
	if obj := (Override{}).Object(); len(obj) != 0 {
		t.Errorf("an empty override is stored as {}, got %v", obj)
	}
	if ParseOverride(nil).Guidance != nil {
		t.Error("an absent override has no guidance")
	}
}
