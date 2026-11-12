package pipelines

import "testing"

func TestParseAISettingsIsOffUnlessTurnedOn(t *testing.T) {
	for _, raw := range []string{"", "  ", "null", "{}", "not json", `{"review":"yes"}`, `{"review":true,"typo":true}`, `{"review":true} {"diagnosis":true}`} {
		got := ParseAISettings(raw)
		if raw == "null" || raw == "{}" {
			if got != (AISettings{}) {
				t.Errorf("%q: want all off, got %+v", raw, got)
			}
			continue
		}
		if got != (AISettings{}) {
			t.Errorf("%q: want all off, got %+v", raw, got)
		}
	}
	if got := ParseAISettings(`{"review":true,"diagnosis":false}`); got != (AISettings{Review: true}) {
		t.Errorf("review only: %+v", got)
	}
	if got := ParseAISettings(`{"review":false,"diagnosis":true}`); got != (AISettings{Diagnosis: true}) {
		t.Errorf("diagnosis only: %+v", got)
	}
}

// What a save writes is what the engine reads back, for every position of the
// two switches -- and both keys are always written, in the order the Settings
// page wrote them before the save moved server-side.
func TestTheSavedAISettingsReadBack(t *testing.T) {
	if got := (AISettings{Review: true}).Encode(); got != `{"review":true,"diagnosis":false}` {
		t.Errorf("Encode writes %s; want both keys, review first", got)
	}
	for _, s := range []AISettings{{}, {Review: true}, {Diagnosis: true}, {Review: true, Diagnosis: true}} {
		if got := ParseAISettings(s.Encode()); got != s {
			t.Errorf("%+v was saved as %s, which reads back as %+v", s, s.Encode(), got)
		}
	}
}
