package memql

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
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
	if e.accessAppReadsForTest == nil {
		withAppRows(e)
	}
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

func noReason(t *testing.T, reasons []string, unwanted ...string) {
	t.Helper()
	for _, r := range reasons {
		for _, u := range unwanted {
			if r == u {
				t.Fatalf("reasons = %q, want none to be %q", reasons, u)
			}
		}
	}
}

// machineReasons are every sentence a machine can add.
var machineReasons = []string{
	"no machine of theirs or lent to them offers a model",
	"a machine of theirs is revoked",
	"the machines theirs or lent to them offer only embedding models",
	"no model on the machines theirs or lent to them has structured output and a context of at least 8192",
}

func TestAnOfflineLentMachineCountsAsSetUp(t *testing.T) {
	e := engineWithFleet([]FleetModel{accessModel(accessMachine("lent-laptop", "lender", false))})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceMachine, State: AccessAsleep, Own: false, LenderUserId: "lender", Label: "lent-laptop"}})
	noReason(t, reasons, machineReasons...)
}

func TestTheirOwnOnlineMachineIsReady(t *testing.T) {
	e := engineWithFleet([]FleetModel{accessModel(accessMachine("mine", "ana", true))})
	sources, _ := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceMachine, State: AccessReady, Own: true, Label: "mine"}})
}

// A model that chats and also embeds counts (ruling R-D5); only one that
// cannot answer structured output is out.
func TestAModelThatAlsoEmbedsStillCounts(t *testing.T) {
	m := accessModel(accessMachine("mine", "ana", true))
	m.Embeddings = true
	e := engineWithFleet([]FleetModel{m})
	sources, _ := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceMachine, State: AccessReady, Own: true, Label: "mine"}})
}

func TestAnEmbeddingOnlyModelIsNotAccess(t *testing.T) {
	m := accessModel(accessMachine("mine", "ana", true))
	m.Embeddings, m.StructuredOutput = true, false
	e := engineWithFleet([]FleetModel{m})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "the machines theirs or lent to them offer only embedding models")
	noReason(t, reasons, "no machine of theirs or lent to them offers a model",
		"no model on the machines theirs or lent to them has structured output and a context of at least 8192")
}

func TestAModelBelowTheContextFloorIsNotAccess(t *testing.T) {
	m := accessModel(accessMachine("mine", "ana", true))
	m.ContextWindow = 4096
	e := engineWithFleet([]FleetModel{m})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "no model on the machines theirs or lent to them has structured output and a context of at least 8192")
	noReason(t, reasons, "no machine of theirs or lent to them offers a model",
		"the machines theirs or lent to them offer only embedding models")
}

// One machine with an embedding model and another with a chat model below
// the floor: not every model only embeds, so the floor is the reason.
func TestAChatModelBelowTheFloorBesideAnEmbedderIsTheFloorReason(t *testing.T) {
	embedder := accessModel(accessMachine("indexer", "ana", true))
	embedder.ModelId, embedder.Embeddings, embedder.StructuredOutput = "embed", true, false
	small := accessModel(accessMachine("desk", "ana", true))
	small.ModelId, small.ContextWindow = "small", 2048
	_, reasons := accessSourcesFor(t, engineWithFleet([]FleetModel{embedder, small}), "ana")
	wantReason(t, reasons, "no model on the machines theirs or lent to them has structured output and a context of at least 8192")
	noReason(t, reasons, "the machines theirs or lent to them offer only embedding models")
}

// No machine at all, theirs or lent, is its own reason.
func TestNoMachineAtAllIsItsOwnReason(t *testing.T) {
	sources, reasons := accessSourcesFor(t, engineWithFleet(nil), "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "no machine of theirs or lent to them offers a model")
	noReason(t, reasons, "a machine of theirs is revoked",
		"the machines theirs or lent to them offer only embedding models",
		"no model on the machines theirs or lent to them has structured output and a context of at least 8192")
}

// Final review, minor 3: each machine is judged by what IT advertised. The
// model's union says 8192 and structured output because a REVOKED machine
// said so; the live machine's own window is 4096, so it does not count.
func TestARevokedQualifyingMachineDoesNotMakeAnotherCount(t *testing.T) {
	qualifying := accessMachine("old", "ana", false)
	qualifying.Revoked = true
	qualifying.Capabilities, qualifying.ContextWindow = FleetModelCapabilities{StructuredOutput: true}, 8192
	small := accessMachine("small", "ana", true)
	small.Capabilities, small.ContextWindow = FleetModelCapabilities{StructuredOutput: true}, 4096
	m := accessModel(qualifying, small)
	if !chatModelMeetsMinimum(m) {
		t.Fatal("fixture: the model's union must qualify, or the test proves nothing")
	}
	sources, reasons := accessSourcesFor(t, engineWithFleet([]FleetModel{m}), "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "a machine of theirs is revoked")
	wantReason(t, reasons, "no model on the machines theirs or lent to them has structured output and a context of at least 8192")
}

// Final review, minor 10: one machine offering two qualifying models is one
// source, not two.
func TestAMachineOfferingTwoQualifyingModelsIsOneSource(t *testing.T) {
	first := accessModel(accessMachine("desk", "ana", true))
	first.ModelId = "first"
	second := accessModel(accessMachine("desk", "ana", true))
	second.ModelId = "second"
	sources, _ := accessSourcesFor(t, engineWithFleet([]FleetModel{first, second}), "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceMachine, State: AccessReady, Own: true, Label: "desk"}})
}

// appRow is a worker registration as the app read sees it.
func appRow(owner string, apps ...map[string]any) map[string]any {
	entries := make([]any, 0, len(apps))
	for _, a := range apps {
		entries = append(entries, a)
	}
	return map[string]any{"id": "v1:worker:registration:desk", "ownerUserId": owner, "name": "Desk", "connectedNodeId": "agent-1", "apps": entries}
}

func appEntry(id, readiness string) map[string]any {
	e := map[string]any{"id": id, "allowed": true, "signedIn": true}
	if readiness != "" {
		e["readiness"] = readiness
	}
	return e
}

type blockAnswer struct {
	until     time.Time
	signedOut bool
}

// withAppRows answers desktop access's app reads from rows and, per app, a
// session-evidence block.
func withAppRows(e *MemQLEngine, rows ...map[string]any) {
	withAppRowsAndBlocks(e, nil, rows...)
}

func withAppRowsAndBlocks(e *MemQLEngine, blocks map[string]blockAnswer, rows ...map[string]any) {
	e.accessAppReadsForTest = &accessAppReads{
		Rows: func(context.Context, string) ([]map[string]any, error) { return rows, nil },
		Block: func(_ context.Context, _ string, app string, _ bool, _ time.Time) (time.Time, bool, error) {
			b := blocks[app]
			return b.until, b.signedOut, nil
		},
	}
}

func appSourcesOf(t *testing.T, e *MemQLEngine) []AccessSource {
	t.Helper()
	sources, _ := accessSourcesFor(t, e, "ana")
	var apps []AccessSource
	for _, s := range sources {
		if s.Kind == AccessSourceApp {
			apps = append(apps, s)
		}
	}
	return apps
}

func TestEveryAppStateIsPinned(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := []struct {
		name      string
		row       map[string]any
		blocks    map[string]blockAnswer
		wantState string
	}{
		{"ready and online", appRow("ana", appEntry("codex", "ready")), nil, AccessReady},
		{"never measured and online", appRow("ana", appEntry("codex", "")), nil, AccessReady},
		{"unknown counts and is ready online", appRow("ana", appEntry("codex", "unknown")), nil, AccessReady},
		{"out of usage measured", appRow("ana", appEntry("codex", "out_of_usage")), nil, AccessOutOfUsage},
		{"usage block from a session", appRow("ana", appEntry("codex", "")), map[string]blockAnswer{"codex": {until: future}}, AccessOutOfUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := engineWithFleet(nil)
			withAppRowsAndBlocks(e, tc.blocks, tc.row)
			wantSources(t, appSourcesOf(t, e), []AccessSource{{Kind: AccessSourceApp, App: "codex", State: tc.wantState, Own: true, Label: "codex on Desk"}})
		})
	}

	t.Run("asleep when no stream is held", func(t *testing.T) {
		row := appRow("ana", appEntry("claude-code", "ready"))
		row["connectedNodeId"] = ""
		e := engineWithFleet(nil)
		withAppRows(e, row)
		wantSources(t, appSourcesOf(t, e), []AccessSource{{Kind: AccessSourceApp, App: "claude-code", State: AccessAsleep, Own: true, Label: "claude-code on Desk"}})
	})
	t.Run("unknown on a machine that is asleep is asleep", func(t *testing.T) {
		row := appRow("ana", appEntry("codex", "unknown"))
		delete(row, "connectedNodeId")
		e := engineWithFleet(nil)
		withAppRows(e, row)
		wantSources(t, appSourcesOf(t, e), []AccessSource{{Kind: AccessSourceApp, App: "codex", State: AccessAsleep, Own: true, Label: "codex on Desk"}})
	})
}

func TestAnAppIsNotSetUpWhen(t *testing.T) {
	future := time.Now().Add(time.Hour)
	revoked := appRow("ana", appEntry("codex", "ready"))
	revoked["revokedAt"] = "2026-11-01T00:00:00Z"
	appsOff := appRow("ana", appEntry("codex", "ready"))
	appsOff["ownerFeatures"] = map[string]any{"apps": false}
	routedAway := appRow("ana", appEntry("codex", "ready"))
	routedAway["appRouting"] = map[string]any{"mode": "claude-code"}
	notAllowed := appEntry("codex", "ready")
	notAllowed["allowed"] = false
	notSignedIn := appEntry("codex", "ready")
	notSignedIn["signedIn"] = false
	cases := []struct {
		name   string
		row    map[string]any
		blocks map[string]blockAnswer
	}{
		{"its registration is revoked", revoked, nil},
		{"apps are switched off for the machine", appsOff, nil},
		{"the person's app routing disallows it", routedAway, nil},
		{"its id is not in the runnable set", appRow("ana", appEntry("gemini", "ready")), nil},
		{"the machine does not allow it", appRow("ana", notAllowed), nil},
		{"it is not signed in", appRow("ana", notSignedIn), nil},
		{"it was measured signed out", appRow("ana", appEntry("codex", "signed_out")), nil},
		{"a session was refused as signed out", appRow("ana", appEntry("codex", "")), map[string]blockAnswer{"codex": {until: future, signedOut: true}}},
		{"the machine is somebody else's", appRow("ben", appEntry("codex", "ready")), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := engineWithFleet(nil)
			withAppRowsAndBlocks(e, tc.blocks, tc.row)
			sources, reasons := accessSourcesFor(t, e, "ana")
			for _, s := range sources {
				if s.Kind == AccessSourceApp {
					t.Fatalf("sources = %+v, want no app", sources)
				}
			}
			wantReason(t, reasons, "no app on their machines is allowed and signed in")
		})
	}
}

// A refusal that has already ended no longer blocks the app.
func TestAnExpiredSessionBlockDoesNotChangeAnApp(t *testing.T) {
	e := engineWithFleet(nil)
	withAppRowsAndBlocks(e, map[string]blockAnswer{"codex": {until: time.Now().Add(-time.Hour), signedOut: true}}, appRow("ana", appEntry("codex", "")))
	wantSources(t, appSourcesOf(t, e), []AccessSource{{Kind: AccessSourceApp, App: "codex", State: AccessReady, Own: true, Label: "codex on Desk"}})
}

func TestAnAppRowReadThatFailsIsAnError(t *testing.T) {
	e := engineWithFleet(nil)
	e.accessAppReadsForTest = &accessAppReads{Rows: func(context.Context, string) ([]map[string]any, error) { return nil, errors.New("down") }}
	if _, _, err := e.desktopAccessSources(personContext("ana"), "ana"); err == nil {
		t.Fatal("a failed registration read must be an error, not an empty answer")
	}
}

func TestARevokedMachineIsNotAMachineSetUp(t *testing.T) {
	m := accessMachine("old", "ana", false)
	m.Revoked = true
	e := engineWithFleet([]FleetModel{accessModel(m)})
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "a machine of theirs is revoked")
	// The revoked machine is the whole story: it is not also "no machine".
	noReason(t, reasons, "no machine of theirs or lent to them offers a model")
}

// A revoked machine lent to them names its lender, by bare id, as a key lent
// to them does.
func TestARevokedLentMachineNamesItsLender(t *testing.T) {
	m := accessMachine("lent", "v1:identity:user:lender", true)
	m.Revoked = true
	sources, reasons := accessSourcesFor(t, engineWithFleet([]FleetModel{accessModel(m)}), "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "a machine lent by lender is revoked")
	noReason(t, reasons, "a machine of theirs is revoked")
}

func TestAnEngineWithNoProvidersIsAnErrorNotNothingSetUp(t *testing.T) {
	if _, _, err := (&MemQLEngine{}).desktopAccessSources(personContext("ana"), "ana"); err == nil {
		t.Fatal("want an error")
	}
}

func TestAKeyReadThatFailsIsAnErrorNotAHold(t *testing.T) {
	keys := newFakeVendorKeys()
	keys.keysErr = errors.New("store down")
	e := keyEngine("claude", keys)
	withAppRows(e)
	if _, _, err := e.desktopAccessSources(personContext("ana"), "ana"); err == nil {
		t.Fatal("a key read that failed must be an error, not a reason to hold")
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
	sources, reasons := accessSourcesFor(t, keyEngine("claude", keys), "ana")
	wantSources(t, sources, []AccessSource{{Kind: AccessSourceKey, State: AccessReady, Own: true, Label: "Anthropic"}})
	noReason(t, reasons, "no API key of theirs or lent to them")
}

// No key at all, theirs or lent, is its own reason; a key that exists and does
// not count says why in its own sentence instead.
func TestNoKeyAtAllIsItsOwnReason(t *testing.T) {
	_, reasons := accessSourcesFor(t, keyEngine("claude", noKeys()), "ana")
	wantReason(t, reasons, "no API key of theirs or lent to them")

	paused := newFakeVendorKeys()
	paused.passedOver = []VendorAccountPassedOver{{Account: keyAccount(VendorAnthropic, "k1", "lender"), Reason: VendorKeyPausedReason}}
	_, reasons = accessSourcesFor(t, keyEngine("claude", paused), "ana")
	wantReason(t, reasons, "the key lent by lender is paused by its owner")
	noReason(t, reasons, "no API key of theirs or lent to them")
}

// Final review, minor 4: a key counts only where the cluster would send it.
// With the vendor's calls going to another address, no key is ever used
// (keysRefusedAt, as inferenceStatus applies it), so neither a usable key nor
// one out of credit counts, and the line says why.
func TestAKeyTheClusterWouldNeverSendDoesNotCount(t *testing.T) {
	elsewhere := func(keys *fakeVendorKeys) *MemQLEngine {
		e := engineWithFleet(nil)
		e.providers.setEntry(vendorRecord("claude", VendorAnthropic, false, "https://proxy.internal"))
		e.providers.SetVendorKeys(keys)
		return e
	}
	want := "this cluster sends Anthropic calls to another address, so API keys cannot be used here"

	sources, reasons := accessSourcesFor(t, elsewhere(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana"))), "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, want)
	noReason(t, reasons, "no API key of theirs or lent to them")

	credit := newFakeVendorKeys()
	credit.passedOver = []VendorAccountPassedOver{{Account: keyAccount(VendorAnthropic, "k1", "lender"), Reason: VendorKeyCreditReason(VendorAnthropic)}}
	sources, reasons = accessSourcesFor(t, elsewhere(credit), "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, want)

	// One record of the vendor that dials its own address is enough.
	e := elsewhere(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))
	e.providers.setEntry(vendorRecord("claude-direct", VendorAnthropic, false, ""))
	sources, _ = accessSourcesFor(t, e, "ana")
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
	noReason(t, reasons, "may not use the cluster's account")
}

// Final review, Important 3: a person the cluster's account is closed to is
// told so, whether or not an account is set up; granting the permission is
// the administrator's fix.
func TestNoPermissionToTheClusterAccountIsItsOwnReason(t *testing.T) {
	closed := func(context.Context, string) bool { return false }

	e := engineWithFleet(nil)
	e.providers.SetClusterAccountGate(closed)
	_, reasons := accessSourcesFor(t, e, "ana")
	wantReason(t, reasons, "may not use the cluster's account")
	noReason(t, reasons, "may use the cluster's account, but none is set up")

	// An account set up, and closed to them.
	e = engineWithFleet(nil)
	e.providers.setEntry(federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, "")))
	e.providers.SetVendorKeys(noKeys())
	e.providers.SetClusterAccountGate(closed)
	sources, reasons := accessSourcesFor(t, e, "ana")
	wantSources(t, sources, nil)
	wantReason(t, reasons, "may not use the cluster's account")
}

// Several causes stand together, in one order, so the same state always reads
// and digests the same.
func TestSeveralCausesStandTogetherInOneOrder(t *testing.T) {
	e := engineWithFleet(nil)
	e.providers.SetClusterAccountGate(func(context.Context, string) bool { return false })
	_, first := accessSourcesFor(t, e, "ana")
	want := []string{
		"may not use the cluster's account",
		"no API key of theirs or lent to them",
		"no app on their machines is allowed and signed in",
		"no machine of theirs or lent to them offers a model",
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("reasons = %q, want %q", first, want)
	}
	_, again := accessSourcesFor(t, e, "ana")
	if !reflect.DeepEqual(first, again) || accessReasonsDigest(first) != accessReasonsDigest(again) {
		t.Fatalf("the same state read %q, then %q", first, again)
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

// The builtin answers in ONE row (id "current"), for the caller alone: the
// person comes from the actor, never from an argument (design section 3).
func TestDesktopAccessIsExactlyOneRow(t *testing.T) {
	installRecorder(t)
	e := holdEngine(auth.RoleWriter)
	nodes, err := e.evaluateDesktopAccess(personContext("ana"), nil, 0)
	if err != nil {
		t.Fatalf("desktopAccess: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "current" || nodes[0].Concept != DesktopAccessConcept {
		t.Fatalf("nodes = %+v, want exactly one row with a constant id on %s", nodes, DesktopAccessConcept)
	}
	p := decodePayload(t, nodes[0].Payload)
	if _, ok := p["admitted"]; !ok {
		t.Fatalf("payload %v has no admitted", p)
	}
	if _, ok := p["exempt"]; !ok {
		t.Fatalf("payload %v has no exempt", p)
	}
	if p["admitted"] != false || p["exempt"] != false {
		t.Fatalf("payload %v, want a held writer", p)
	}
	if ref, _ := p["reference"].(string); ref == "" {
		t.Fatalf("payload %v, want the held answer's reference", p)
	}
}

// A call that names nobody is an error, and so is any error from the decision:
// the client reads an error as unknown and lets the person in, so it must never
// be dressed up as a row.
func TestDesktopAccessWithNoPersonIsAnError(t *testing.T) {
	e := holdEngine(auth.RoleWriter)
	if nodes, err := e.evaluateDesktopAccess(context.Background(), nil, 0); err == nil {
		t.Fatalf("nodes = %+v, want an error for a call that names nobody", nodes)
	}
}

// The row never carries a person argument: an argument naming someone else is
// ignored, the actor decides.
func TestDesktopAccessIgnoresAnArgumentNamingAnotherPerson(t *testing.T) {
	installRecorder(t)
	e := holdEngine(auth.RoleWriter)
	e.storedRoleForTest = func(_ context.Context, id string) (auth.Role, bool, error) {
		if id == "boss" {
			return auth.RoleOwner, true, nil
		}
		return auth.RoleWriter, true, nil
	}
	nodes, err := e.evaluateDesktopAccess(personContext("ana"), map[string]any{"userId": "boss", "actingUserId": "boss"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p := decodePayload(t, nodes[0].Payload); p["exempt"] != false {
		t.Fatalf("payload %v: a named person must not change whose access is read", p)
	}
}
