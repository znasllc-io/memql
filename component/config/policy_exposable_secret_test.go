package config

import (
	"strings"
	"testing"

	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

// policy_exposable_secret_test.go is the regression guard for memql#3188.
//
// The `go/clear-text-logging` CodeQL family on this repo reached 492 open
// alerts on main, and ALL 492 traced to one three-step root:
//
//	policy_exposable.go  "selection of SiOpenaiApiKey"
//	  >> policy_exposable.go  "call to readConfigField"
//	  >> policy_exposable.go  "v"            (the `case bool` branch)
//
// readConfigField merged six differently-typed fields behind one `any`
// return, so static analysis could not refine the dynamic type and admitted a
// `case bool` branch that is provably unreachable for a field declared
// `string`. From that single false step, field-insensitive taint carried the
// key into every downstream logging call in the engine -- a median 84-hop
// flow, 103 distinct logged expressions, not one of them a credential.
//
// The fix reads Sensitive entries through readSensitivePresence, which
// returns bool, so the raw value has no path into the ctx map at all. These
// tests pin the property the fix establishes, so the family cannot come back
// the way it arrived: it went 4 -> 492 on a single unrelated commit that added
// one edge into the ambient envelope, and any future such edge would
// regenerate it if the source were still tainted.

const probeKey = "sk-live-CONFIG-MUST-NOT-EXPOSE-0123456789"

// A Sensitive entry resolves to a bool -- never to the value, and never to a
// string at all -- whether it is set or unset.
func TestSensitiveConfigIsPresenceOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot *busv1.ConfigSnapshot
		want     bool
	}{
		{"set", &busv1.ConfigSnapshot{SiOpenaiApiKey: probeKey}, true},
		{"unset", &busv1.ConfigSnapshot{SiOpenaiApiKey: ""}, false},
		{"whitespace only", &busv1.ConfigSnapshot{SiOpenaiApiKey: "   "}, false},
		{"nil snapshot", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := BuildPolicyConfigCtx(tc.snapshot)

			got, ok := ctx["openaiApiKey"]
			if !ok {
				t.Fatal("openaiApiKey is missing from the ctx map -- every " +
					"allow-listed key must always be present so a policy body " +
					"can reference it unconditionally")
			}
			b, isBool := got.(bool)
			if !isBool {
				t.Fatalf("openaiApiKey is %T (%v), want bool.\n\n"+
					"A Sensitive allow-list entry must collapse to presence. "+
					"If this is a string, the raw value is reachable from a "+
					"policy body and the memql#3188 taint path is open again.",
					got, got)
			}
			if b != tc.want {
				t.Errorf("presence = %v, want %v", b, tc.want)
			}
		})
	}
}

// The value must not appear ANYWHERE in the built map -- not under its own
// key, not under any other. This is the assertion that would fail if someone
// re-added SiOpenaiApiKey to readConfigField and dropped the Sensitive flag.
func TestSensitiveConfigValueIsAbsentFromWholeCtx(t *testing.T) {
	ctx := BuildPolicyConfigCtx(&busv1.ConfigSnapshot{
		SiOpenaiApiKey:    probeKey,
		SiDefaultProvider: "chat54Mini",
	})

	for k, v := range ctx {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, probeKey) {
			t.Fatalf("the API key value leaked into the policy ctx under %q.\n\n"+
				"Sensitive entries are presence-only (memql#3188). Read them "+
				"through readSensitivePresence, not readConfigField.", k)
		}
	}

	// Control: a NON-sensitive entry still carries its real value, so the
	// guard above is targeted rather than blanket.
	if ctx["defaultProvider"] != "chat54Mini" {
		t.Errorf("defaultProvider = %v, want \"chat54Mini\" -- non-sensitive "+
			"config must still expose its value", ctx["defaultProvider"])
	}
}

// readConfigField must not be able to return a Sensitive field's raw value.
// This is the structural half of the fix: the presence collapse is correct
// because of the reader's TYPE, not because of a runtime type switch that a
// static analyser cannot follow.
func TestReadConfigFieldCannotReturnSensitiveValues(t *testing.T) {
	snapshot := &busv1.ConfigSnapshot{SiOpenaiApiKey: probeKey}

	for _, f := range PolicyExposableConfig {
		if !f.Sensitive {
			continue
		}
		if got := readConfigField(snapshot, f.FieldName); got != nil {
			t.Errorf("readConfigField(%q) returned %T(%v), want nil.\n\n"+
				"Sensitive fields must be absent from readConfigField's switch "+
				"so its `any` return can never carry credential material. That "+
				"merged `any` is what let CodeQL admit an unreachable branch and "+
				"report 492 alerts (memql#3188).", f.FieldName, got, got)
		}
		if !readSensitivePresence(snapshot, f.FieldName) {
			t.Errorf("readSensitivePresence(%q) = false for a set field -- the "+
				"Sensitive entry has no reader, so its ctx key is stuck at false",
				f.FieldName)
		}
	}
}
