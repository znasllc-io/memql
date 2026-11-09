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
