package pipelines

import "testing"

func TestSecurityConsentIsExplicitAndFailClosed(t *testing.T) {
	for _, raw := range []string{"", "true", `[]`, `{`, `{"enabled":"true"}`, `{"enabled":true,"typo":1}`, `{"enabled":true} {}`, `{"enabled":true} garbage`, `null`, `{"enabled":null}`, `{}`, `{"enabled":true,"enabled":null}`, `{"enabled":true,"ENABLED":true}`} {
		if ParseSecuritySettings(raw).Enabled {
			t.Fatalf("invalid/off setting enabled scans: %s", raw)
		}
	}
	for _, raw := range []string{`{"enabled":true}`, ` {"ENABLED":true} `} {
		if !ParseSecuritySettings(raw).Enabled {
			t.Fatalf("explicit consent was lost: %s", raw)
		}
	}
}

// What a save writes is what the engine reads back, for every position of the
// switch -- and it is the same text the Settings page wrote before the save
// moved server-side, so a row either of them wrote reads the same.
func TestTheSavedSecuritySettingReadsBack(t *testing.T) {
	for enabled, want := range map[bool]string{true: `{"enabled":true}`, false: `{"enabled":false}`} {
		got := SecuritySettings{Enabled: enabled}.Encode()
		if got != want {
			t.Errorf("Encode(enabled=%v) = %s, want %s", enabled, got, want)
		}
		if ParseSecuritySettings(got).Enabled != enabled {
			t.Errorf("%s does not read back as enabled=%v", got, enabled)
		}
	}
}
