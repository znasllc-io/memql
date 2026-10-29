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
