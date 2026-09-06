package work

import "testing"

func TestClassifyByRules_TransientSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal Signal
		rule   string
	}{
		{"connection refused", Signal{ErrorMessage: "dial tcp 10.0.0.1:443: connect: connection refused"}, "transient.network"},
		{"timeout", Signal{ErrorMessage: "context deadline exceeded"}, "transient.timeout"},
		{"rate limit by code", Signal{ErrorCode: "rate_limited"}, "transient.rateLimit"},
		{"429", Signal{ErrorMessage: "provider returned 429 Too Many Requests"}, "transient.rateLimit"},
		{"503", Signal{ErrorMessage: "upstream returned 503 Service Unavailable"}, "transient.unavailable"},
	} {
		sym, ev, ok := ClassifyByRules(tc.signal)
		if !ok {
			t.Errorf("%s: rules had no opinion; a rules miss costs a model call", tc.name)
			continue
		}
		if sym != SymptomTransient {
			t.Errorf("%s: symptom = %q, want transient", tc.name, sym)
		}
		if ev.RuleId != tc.rule {
			t.Errorf("%s: ruleId = %q, want %q", tc.name, ev.RuleId, tc.rule)
		}
		if ev.Source != EvidenceSourceRules {
			t.Errorf("%s: source = %q, want rules", tc.name, ev.Source)
		}
	}
}

func TestClassifyByRules_EnvironmentSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal Signal
		rule   string
	}{
		{"permission", Signal{ErrorMessage: "permission denied writing /etc/hosts"}, "environment.permission"},
		{"forbidden code", Signal{ErrorCode: "forbidden"}, "environment.permission"},
		{"not found", Signal{ErrorMessage: "open /home/a/report.csv: no such file or directory"}, "environment.notFound"},
		{"404", Signal{ErrorMessage: "GET https://example.com/x returned 404"}, "environment.notFound"},
		{"literal does not hold", Signal{PreconditionFailed: true, ErrorMessage: "precondition literal /opt/tool is absent on this machine"}, "environment.literal"},
	} {
		sym, ev, ok := ClassifyByRules(tc.signal)
		if !ok {
			t.Errorf("%s: rules had no opinion", tc.name)
			continue
		}
		if sym != SymptomEnvironment {
			t.Errorf("%s: symptom = %q, want environment", tc.name, sym)
		}
		if ev.RuleId != tc.rule {
			t.Errorf("%s: ruleId = %q, want %q", tc.name, ev.RuleId, tc.rule)
		}
	}
}

func TestClassifyByRules_ContractIsAViolatedPostcondition(t *testing.T) {
	sym, ev, ok := ClassifyByRules(Signal{PostconditionFailed: true, ErrorMessage: "row v1:x:y:1 not found after mutation"})
	if !ok || sym != SymptomContract {
		t.Fatalf("symptom = %q ok=%v, want contract", sym, ok)
	}
	if ev.RuleId != "contract.postcondition" {
		t.Errorf("ruleId = %q", ev.RuleId)
	}
}

// A repeated action is the stall signal the debugging papers converged on, and
// it OUTRANKS a transient-looking message: retrying something that has already
// been retried identically is the loop the run-wide budget exists to stop.
func TestClassifyByRules_RepeatedActionEscalatesOverTransient(t *testing.T) {
	sym, ev, ok := ClassifyByRules(Signal{RepeatedAction: true, ErrorMessage: "connection refused"})
	if !ok {
		t.Fatal("rules had no opinion on a stalled step")
	}
	if sym != SymptomHuman {
		t.Fatalf("symptom = %q, want human: a stalled step escalates", sym)
	}
	if ev.RuleId != "human.stalled" {
		t.Errorf("ruleId = %q, want human.stalled", ev.RuleId)
	}
}

func TestClassifyByRules_NoOpinionIsTheModelCall(t *testing.T) {
	if _, _, ok := ClassifyByRules(Signal{ErrorMessage: "the vendor said something nobody has a rule for"}); ok {
		t.Fatal("rules claimed an opinion they do not have; that would skip classifySymptom and mis-act")
	}
	if _, _, ok := ClassifyByRules(Signal{}); ok {
		t.Fatal("an empty signal is not classifiable")
	}
}

func TestActFor_TheFiveActs(t *testing.T) {
	for _, tc := range []struct {
		sym     Symptom
		attempt int
		max     int
		want    Act
	}{
		{SymptomTransient, 1, 3, ActRetry},
		{SymptomTransient, 3, 3, ActAsk}, // budget exhausted -> a person
		{SymptomTransient, 9, 3, ActAsk},
		{SymptomEnvironment, 1, 3, ActHeal},
		{SymptomContract, 1, 3, ActRepair},
		{SymptomPlan, 1, 3, ActReplan},
		{SymptomHuman, 1, 3, ActAsk},
	} {
		if got := ActFor(tc.sym, tc.attempt, tc.max); got != tc.want {
			t.Errorf("ActFor(%s, attempt=%d, max=%d) = %q, want %q", tc.sym, tc.attempt, tc.max, got, tc.want)
		}
	}
}

// D5: never a silent edit. Healing proposes; a person approves.
func TestActFor_EnvironmentNeverEditsSilently(t *testing.T) {
	if ApprovalKindFor(ActHeal) != ApprovalKindPlanReview {
		t.Fatal("an environment heal must raise a planReview approval (spec D5: never a silent edit)")
	}
	if ApprovalKindFor(ActAsk) != ApprovalKindFeedback {
		t.Fatal("asking a person is a feedback approval")
	}
	if ApprovalKindFor(ActRetry) != "" {
		t.Fatal("a retry asks nobody")
	}
}

func TestSymptomIsValid(t *testing.T) {
	for _, s := range AllSymptoms() {
		if !s.Valid() {
			t.Errorf("%q is in AllSymptoms but not Valid", s)
		}
	}
	if Symptom("invented").Valid() {
		t.Fatal("an unknown symptom must not validate: the concept enum is closed")
	}
}
