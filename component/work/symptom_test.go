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

// AN EXHAUSTED QUOTA ARRIVES AS A 429, which is the reason this rule has to
// sit above transient.rateLimit rather than anywhere convenient. OpenAI
// reports a spent balance with the same status it uses for ordinary rate
// limiting, so under the rate-limit rule the run would back off, retry, spend
// its whole budget rediscovering that the money is still gone, and only then
// ask a person -- with a symptom naming a blip.
func TestClassifyByRules_ExhaustedBudgetOutranksRateLimiting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal Signal
	}{
		{"openai reports quota as a 429", Signal{ErrorMessage: "429 You exceeded your current quota, please check your plan and billing details (insufficient_quota)"}},
		{"anthropic credit balance", Signal{ErrorMessage: "Your credit balance is too low to access the Claude API"}},
		{"our own kill-switch", Signal{ErrorMessage: "memql LLM kill-switch: total spend $20.00 reached. This call was HARD-STOPPED locally"}},
		{"a catalogued code", Signal{ErrorCode: "budget_exhausted"}},
	} {
		sym, ev, ok := ClassifyByRules(tc.signal)
		if !ok {
			t.Errorf("%s: rules had no opinion; money is not a thing to ask a model about", tc.name)
			continue
		}
		if sym != SymptomHuman {
			t.Errorf("%s: symptom = %q, want human: no retry replenishes a balance", tc.name, sym)
		}
		if ev.RuleId != RuleIdBudgetExhausted {
			t.Errorf("%s: ruleId = %q, want %q", tc.name, ev.RuleId, RuleIdBudgetExhausted)
		}
	}

	// AND ORDINARY RATE LIMITING IS UNTOUCHED. The new rule sits above the
	// old one, so the old one still has to fire on its own signals -- a
	// budget rule that swallowed rate limits would stop every blip retrying
	// and park people on questions about money they do not owe.
	sym, ev, ok := ClassifyByRules(Signal{ErrorMessage: "provider returned 429 Too Many Requests"})
	if !ok || sym != SymptomTransient || ev.RuleId != "transient.rateLimit" {
		t.Fatalf("plain rate limiting = %q/%q ok=%v, want transient/transient.rateLimit", sym, ev.RuleId, ok)
	}
}

// A full context window is not a blip either, and it reaches this table only
// when the tool loop's compression already failed to free anything (see
// integrations/agent/context_handoff.go). At that point the same bytes meet
// the same limit on every attempt, so the answer is a person and a smaller
// unit of work.
func TestClassifyByRules_ExhaustedContextWindowNeedsAPerson(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal Signal
	}{
		{"openai", Signal{ErrorMessage: "This model's maximum context length is 128000 tokens (context_length_exceeded)"}},
		{"anthropic", Signal{ErrorMessage: "prompt is too long: 216000 tokens > 200000 maximum"}},
		{"a catalogued code", Signal{ErrorCode: "context_length_exceeded"}},
	} {
		sym, ev, ok := ClassifyByRules(tc.signal)
		if !ok {
			t.Errorf("%s: rules had no opinion", tc.name)
			continue
		}
		if sym != SymptomHuman {
			t.Errorf("%s: symptom = %q, want human", tc.name, sym)
		}
		if ev.RuleId != RuleIdContextExhausted {
			t.Errorf("%s: ruleId = %q, want %q", tc.name, ev.RuleId, RuleIdContextExhausted)
		}
	}
}

// A POSTCONDITION FAILURE IS STILL A FACT and still outranks both new rules.
// Their reason for sitting high is that they wear a transient's clothes; a
// postcondition miss is not a guess about text at all, and moving it below
// them would let a step whose promise broke be filed under money.
func TestClassifyByRules_PostconditionStillOutranksTheExhaustionRules(t *testing.T) {
	sym, ev, ok := ClassifyByRules(Signal{
		PostconditionFailed: true,
		ErrorMessage:        "429 insufficient_quota",
	})
	if !ok || sym != SymptomContract || ev.RuleId != "contract.postcondition" {
		t.Fatalf("got %q/%q ok=%v, want contract/contract.postcondition", sym, ev.RuleId, ok)
	}
}

// A stall still outranks everything, exhaustion included: a step that has
// already been retried identically is the loop the budget exists to stop, and
// what its message happens to say does not change that.
func TestClassifyByRules_StallStillOutranksEverything(t *testing.T) {
	_, ev, ok := ClassifyByRules(Signal{RepeatedAction: true, ErrorMessage: "429 insufficient_quota"})
	if !ok || ev.RuleId != "human.stalled" {
		t.Fatalf("ruleId = %q ok=%v, want human.stalled", ev.RuleId, ok)
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

// The two questions ActAsk can raise are not the same question, and the
// person deciding reads the KIND's sentence rather than the act's. "It cannot
// decide this one on its own" is wrong about a spent balance: the system knows
// exactly what to do and cannot pay for it.
func TestApprovalKindForVerdict_MoneyAsksAboutMoney(t *testing.T) {
	_, budgetEvidence, ok := ClassifyByRules(Signal{ErrorCode: "budget_exhausted"})
	if !ok {
		t.Fatal("the budget rule did not fire")
	}
	if got := ApprovalKindForVerdict(ActAsk, budgetEvidence); got != ApprovalKindBudget {
		t.Fatalf("kind = %q, want %q", got, ApprovalKindBudget)
	}

	// Everything else is unchanged, including the other escalating rules --
	// a stall and a full context window are genuinely "the system does not
	// know how to proceed", which is what `feedback` says.
	_, stallEvidence, _ := ClassifyByRules(Signal{RepeatedAction: true, ErrorMessage: "x"})
	if got := ApprovalKindForVerdict(ActAsk, stallEvidence); got != ApprovalKindFeedback {
		t.Fatalf("a stall raised %q, want %q", got, ApprovalKindFeedback)
	}
	if got := ApprovalKindForVerdict(ActHeal, budgetEvidence); got != ApprovalKindPlanReview {
		t.Fatalf("healing raised %q even with budget evidence, want %q (D5 still holds)", got, ApprovalKindPlanReview)
	}
	if got := ApprovalKindForVerdict(ActRetry, budgetEvidence); got != "" {
		t.Fatalf("a retry raised %q; it asks nobody", got)
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
