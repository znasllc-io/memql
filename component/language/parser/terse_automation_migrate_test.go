package parser

import (
	"strings"
	"testing"
)

func TestRewriteLonghandSingleStepAutomation(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "canonical pass-through collapses",
			in: `@trigger(event="system.startup")
automation registerNode {
  step run {
    logic registerNode { event: event }
  }
}`,
			want: `automation registerNode @trigger(event="system.startup") => logic registerNode`,
		},
		{
			name: "other annotations stay above the terse line",
			in: `@description("Sweep expired rows nightly.")
@trigger(schedule="0 3 * * *")
automation nightlySweep {
  step run {
    logic nightlySweep { event: event }
  }
}`,
			want: `@description("Sweep expired rows nightly.")
automation nightlySweep @trigger(schedule="0 3 * * *") => logic nightlySweep`,
		},
		{
			name: "multi-step automation untouched",
			in: `@trigger(event="x.y")
automation multi {
  step first {
    logic a { event: event }
  }
  step second {
    logic b { event: event }
  }
}`,
		},
		{
			name: "single step with extra payload keys untouched",
			in: `@trigger(event="x.y")
automation custom {
  step run {
    logic a { event: event, extra: "1" }
  }
}`,
		},
		{
			name: "comment inside the construct preserved by skipping",
			in: `@trigger(event="x.y")
automation commented {
  step run {
    // forwards the payload wholesale
    logic commented { event: event }
  }
}`,
		},
		{
			name: "already terse untouched",
			in:   `automation done @trigger(event="x.y") => logic done`,
		},
	}
	for _, tc := range cases {
		got, err := RewriteLonghandSingleStepAutomation([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := tc.want
		if want == "" {
			want = tc.in
		}
		if string(got) != want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, string(got), want)
		}
	}
}

// TestTerseRewriteLoweringEquivalence pins the story's equivalence
// requirement end to end: the rewritten terse form must normalise to
// the exact procedural text the longhand produced.
func TestTerseRewriteLoweringEquivalence(t *testing.T) {
	longhand := `@trigger(event="system.startup")
automation registerNode {
  step run {
    logic registerNode { event: event }
  }
}`
	got, err := RewriteLonghandSingleStepAutomation([]byte(longhand))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == longhand {
		t.Fatal("canonical pass-through must rewrite")
	}
	lowered, err := NormaliseTerseAutomationSource(string(got))
	if err != nil {
		t.Fatal(err)
	}
	newOut, err := NormaliseAutomationSource(lowered)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, err := NormaliseAutomationSource(longhand)
	if err != nil {
		t.Fatal(err)
	}
	if newOut != oldOut {
		t.Errorf("lowered outputs diverge:\n old %q\n new %q", oldOut, newOut)
	}
	if !strings.Contains(string(got), "=> logic registerNode") {
		t.Errorf("terse arrow missing: %q", string(got))
	}
}

// TestRewriteLonghandSingleStep_ParenForm pins the corpus's actual
// spelling: the paren step-call forward collapses identically.
func TestRewriteLonghandSingleStep_ParenForm(t *testing.T) {
	in := `@trigger(schedule="0 4 * * *")
automation auditEventRetentionSweep {
  step run {
    logic auditEventRetentionSweep ( event: event )
  }
}`
	want := `automation auditEventRetentionSweep @trigger(schedule="0 4 * * *") => logic auditEventRetentionSweep`
	got, err := RewriteLonghandSingleStepAutomation([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("paren form:\n got %q\nwant %q", string(got), want)
	}
}

// TestRewriteLonghandSingleStep_TriggerFirstOrder pins the corpus's
// dominant annotation order (@trigger first, @description second) --
// the hoist flips the pair and the equivalence check must treat the
// leading annotation run as a set.
func TestRewriteLonghandSingleStep_TriggerFirstOrder(t *testing.T) {
	in := `@trigger(schedule="0 5 9 * * *")
@description("Daily reminder sweep.")
automation reminder25 {
  step run {
    logic reminder25 ( event: event )
  }
}`
	want := `@description("Daily reminder sweep.")
automation reminder25 @trigger(schedule="0 5 9 * * *") => logic reminder25`
	got, err := RewriteLonghandSingleStepAutomation([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("trigger-first order:\n got %q\nwant %q", string(got), want)
	}
}
