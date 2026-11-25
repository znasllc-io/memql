package memql

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The sources a person has set up (desktop-access design, section 1). Each test
// builds one engine and asks desktopAccessSources what it counts.

func accessModel(machines ...FleetMachine) FleetModel {
	return FleetModel{ModelId: "m", ContextWindow: 8192, StructuredOutput: true, Machines: machines}
}

func accessMachine(id, owner string, online bool) FleetMachine {
	return FleetMachine{RegistrationId: id, Name: id, OwnerUserId: owner, Online: online, MaxConcurrent: 1}
}

func accessSourcesFor(t *testing.T, e *MemQLEngine, who string) ([]AccessSource, []string) {
	t.Helper()
	sources, reasons, err := e.desktopAccessSources(personContext(who), who)
	if err != nil {
		t.Fatalf("desktopAccessSources: %v", err)
	}
	return sources, reasons
}

func wantSources(t *testing.T, got, want []AccessSource) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
}

func wantReason(t *testing.T, reasons []string, want string) {
	t.Helper()
	for _, r := range reasons {
		if r == want {
			return
		}
	}
	t.Fatalf("reasons = %q, want one to be %q", reasons, want)
}

func TestAnOfflineLentMachineCountsAsSetUp(t *testing.T) {
	e := engineWithFleet([]FleetModel{accessModel(accessMachine("lent-laptop", "lender", false))})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceMachine, State: AccessAsleep, Own: false, LenderUserId: "lender", Label: "lent-laptop"}})
	for _, r := range reasons {
		if strings.Contains(r, "chat model") {
			t.Fatalf("reasons = %q, want none about machines", reasons)
		}
	}
}

func TestTheirOwnOnlineMachineIsReady(t *testing.T) {
	e := engineWithFleet([]FleetModel{accessModel(accessMachine("mine", "ana", true))})
	sources, _ := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceMachine, State: AccessReady, Own: true, Label: "mine"}})
}

func TestAnEmbeddingOnlyModelIsNotAccess(t *testing.T) {
	m := accessModel(accessMachine("mine", "ana", true))
	m.Embeddings, m.StructuredOutput = true, false
	e := engineWithFleet([]FleetModel{m})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "no machine of theirs or lent to them offers a chat model")
}

func TestAModelBelowTheContextFloorIsNotAccess(t *testing.T) {
	m := accessModel(accessMachine("mine", "ana", true))
	m.ContextWindow = 4096
	e := engineWithFleet([]FleetModel{m})
	sources, _ := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
}

func appEngine(door AppDoor) *MemQLEngine {
	e := engineWithFleet(nil)
	e.providers.SetAppInference(&stubApps{doors: []AppDoor{door}})
	return e
}

func TestAnAppOnAnOfflineMachineCountsAsSetUp(t *testing.T) {
	e := appEngine(AppDoor{AppId: appIdClaudeCode, Machines: []AppMachine{{RegistrationId: "laptop", Name: "laptop", Online: false}}})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceApp, State: AccessAsleep, Own: true, Label: "claude-code on laptop"}})
	for _, r := range reasons {
		if strings.Contains(r, "no app") {
			t.Fatalf("reasons = %q, want none about apps", reasons)
		}
	}
}

// Out of usage is a state an app comes back from, like a machine asleep, so it
// counts (ruling R-D2); a later message depends on telling it from asleep.
func TestAnAppOutOfUsageCountsAsSetUp(t *testing.T) {
	e := appEngine(AppDoor{AppId: appIdCodex, Machines: []AppMachine{{RegistrationId: "laptop", Name: "laptop", Online: true, OutOfUsage: true}}})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceApp, State: AccessOutOfUsage, Own: true, Label: "codex on laptop"}})
	for _, r := range reasons {
		if strings.Contains(r, "no app") {
			t.Fatalf("reasons = %q, want none about apps", reasons)
		}
	}
}

func TestAnOutOfUsageMachineIsNotReachableByTheChatDoor(t *testing.T) {
	d := AppDoor{AppId: appIdCodex, Machines: []AppMachine{{RegistrationId: "laptop", Online: true, LocalStream: true, StructuredResult: true, OutOfUsage: true}}}
	if d.Reachable() || d.Runnable() || d.SupportsStructured() {
		t.Fatal("an app that is out of usage must not be reachable, runnable or structured")
	}
}

func keyEngine(vendorEntryName string, keys *fakeVendorKeys) *MemQLEngine {
	e := engineWithFleet(nil)
	e.providers.setEntry(vendorRecord(vendorEntryName, VendorAnthropic, false, ""))
	e.providers.SetVendorKeys(keys)
	return e
}

func TestACreditHeldKeyCountsAndAPausedOneDoesNot(t *testing.T) {
	keys := newFakeVendorKeys()
	keys.passedOver = []VendorAccountPassedOver{{
		Account: keyAccount(VendorAnthropic, "k1", "lender"),
		Reason:  VendorKeyCreditReason(VendorAnthropic),
	}}
	sources, _ := accessSourcesFor(t, keyEngine("claude", keys), "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceKey, State: AccessOutOfCredit, LenderUserId: "lender", Label: "Anthropic"}})

	keys = newFakeVendorKeys()
	keys.passedOver = []VendorAccountPassedOver{{
		Account: keyAccount(VendorAnthropic, "k1", "lender"),
		Reason:  VendorKeyPausedReason,
	}}
	sources, reasons := accessSourcesFor(t, keyEngine("claude", keys), "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "the key lent by lender is paused by its owner")
}

func TestAKeyThatCannotPayExplainsItself(t *testing.T) {
	cases := []struct {
		name, reason, account, want string
	}{
		{"lent refused", VendorKeyRefusedReason(VendorAnthropic), "lender", "the key lent by lender was refused by Anthropic"},
		{"lent owner gone", VendorKeyOwnerInactiveReason, "lender", "the key lent by lender belongs to someone who is no longer an active user"},
		{"own refused", VendorKeyRefusedReason(VendorAnthropic), "ana", "their own key was refused by Anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := newFakeVendorKeys()
			keys.passedOver = []VendorAccountPassedOver{{Account: keyAccount(VendorAnthropic, "k1", tc.account), Reason: tc.reason}}
			sources, reasons := accessSourcesFor(t, keyEngine("claude", keys), "ana")
			wantSources(t, sources, nil)
			wantReason(t, reasons, tc.want)
		})
	}
}

func TestAUsableKeyIsReadyAndOwnWhenTheirs(t *testing.T) {
	keys := keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana"))
	sources, _ := accessSourcesFor(t, keyEngine("claude", keys), "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceKey, State: AccessReady, Own: true, Label: "Anthropic"}})
}

func TestTheClusterAccountCountsWhereItPaysForThePerson(t *testing.T) {
	e := engineWithFleet(nil)
	e.providers.setEntry(federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, "")))
	e.providers.SetVendorKeys(noKeys())
	e.providers.SetClusterAccountGate(openTo("ana").gate)
	sources, _ := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceClusterAccount, State: AccessReady, Label: "the cluster's account"}})
}

func TestPermissionWithNoClusterAccountIsItsOwnReason(t *testing.T) {
	e := engineWithFleet(nil)
	e.providers.SetClusterAccountGate(func(context.Context, string) bool { return true })
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "may use the cluster's account, but none is set up")
}

func TestNoPermissionToTheClusterAccountAddsNoReason(t *testing.T) {
	e := engineWithFleet(nil)
	e.providers.SetClusterAccountGate(func(context.Context, string) bool { return false })
	_, reasons := accessSourcesFor(t, e, "ana")
	for _, r := range reasons {
		if strings.Contains(r, "cluster") {
			t.Fatalf("reasons = %q, want none about the cluster's account", reasons)
		}
	}
}

func TestAnAppReasonIsGivenWhenNoAppCounts(t *testing.T) {
	_, reasons := accessSourcesFor(t, engineWithFleet(nil), "ana")
	wantReason(t, reasons, "no app on their machines is allowed and signed in")
}

func TestTheDigestIgnoresReasonOrder(t *testing.T) {
	a, b := "first reason", "second reason"
	one, two := accessReasonsDigest([]string{a, b}), accessReasonsDigest([]string{b, a})
	if one != two {
		t.Fatalf("digests differ: %q vs %q", one, two)
	}
	if len(one) != 16 {
		t.Fatalf("digest %q has %d characters, want 16", one, len(one))
	}
	if accessReasonsDigest([]string{a}) == one {
		t.Fatal("different reasons must give a different digest")
	}
}
