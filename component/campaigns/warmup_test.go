package campaigns

import (
	"strings"
	"testing"
	"time"
)

// warmup_test.go -- memql#3462. The issue's real content is a refusal to
// build a ramp on a clock, so the tests are about the conditions that stop
// one advancing rather than about it advancing:
//
//	counters exist and are per domain   TestReputationCountersArePerDomain
//	a clean rate over no volume is not  TestCleanRateOverNoVolumeDoesNotAdvance
//	  evidence
//	time alone does not advance it      TestTimeAloneDoesNotAdvance
//	evidence does                       TestGoodEvidenceAdvancesTheRamp
//	one bad domain reduces it           TestOneBadDomainReducesTheRamp
//	  even inside a healthy total       TestHealthyAggregateDoesNotHideABadDomain
//	a tiny domain cannot pin it         TestTinyDomainCannotPinTheRamp
//	it can only ever slow you down      TestRampNeverRaisesPastTheConfiguredRate
//	the operator can read WHY           TestEveryDecisionCarriesItsReason
//	a malformed ladder disables it      TestMalformedLadderLeavesTheRampOff

func warmupWorker(t *testing.T) *Worker {
	t.Helper()
	w := newTestWorker(t, &fakeEngine{}, &recordingSender{})
	steps, err := parseWarmupSteps("5,10,25,50")
	if err != nil {
		t.Fatalf("parseWarmupSteps: %v", err)
	}
	w.cfg.WarmupEnabled = true
	w.cfg.WarmupSteps = steps
	w.cfg.SendingIdentity = "sender@example.test"
	w.cfg.WarmupMinHoursPerStep = 24 * time.Hour
	w.cfg.WarmupMinVolumePerStep = 200
	w.cfg.WarmupMinDomainVolume = 50
	w.cfg.WarmupMaxHardBounceRate = 0.02
	w.cfg.WarmupMaxComplaintRate = 0.001
	return w
}

func rep(domain string, accepted, hard, soft, complaint int) ReputationWindow {
	return ReputationWindow{
		SendingIdentity: "sender@example.test", Domain: domain, WindowStart: "2026-08-01", NodeID: "n1",
		Accepted: accepted, HardBounce: hard, SoftBounce: soft, Complaint: complaint,
	}
}

// TestReputationCountersArePerDomain is the half of the issue that is
// independently valuable: "what is our complaint rate at gmail.com this
// week" is a question an operator asks long before they want a ramp.
func TestReputationCountersArePerDomain(t *testing.T) {
	agg := AggregateReputation([]ReputationWindow{
		// Two replicas, same domain and day: the per-node rows sum.
		rep("gmail.com", 400, 4, 2, 1),
		{SendingIdentity: "sender@example.test", Domain: "gmail.com", WindowStart: "2026-08-01", NodeID: "n2", Accepted: 600, Complaint: 3},
		rep("outlook.com", 200, 0, 0, 0),
	})

	gmail := agg["gmail.com"]
	if gmail.Accepted != 1000 {
		t.Errorf("gmail accepted = %d, want the two replicas' rows summed", gmail.Accepted)
	}
	if got := gmail.ComplaintRate(); got != 0.004 {
		t.Errorf("gmail complaint rate = %v, want 4/1000", got)
	}
	if agg["outlook.com"].ComplaintRate() != 0 {
		t.Error("a healthy domain picked up its neighbour's complaints")
	}
	if agg[""].Accepted != 1200 {
		t.Errorf("the deployment-wide total = %d, want 1200", agg[""].Accepted)
	}
}

func startedState(at time.Time, acceptedWatermark int) WarmupState {
	return WarmupState{Step: 1, RatePerMinute: 10, Decision: "advanced", StepEnteredAt: at, AcceptedAtStepStart: acceptedWatermark}
}

// TestCleanRateOverNoVolumeDoesNotAdvance is the condition the issue is
// really about: four messages with no bounces is a hard bounce rate of 0.0
// and no evidence whatsoever. A ramp that advanced on it would be a
// hardcoded table pretending to be a control loop.
func TestCleanRateOverNoVolumeDoesNotAdvance(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	state := startedState(now.Add(-72*time.Hour), 0) // long past the time minimum

	d := w.evaluateWarmup(state, AggregateReputation([]ReputationWindow{rep("gmail.com", 4, 0, 0, 0)}), now)

	if d.Decision != "held" {
		t.Fatalf("decision = %q, want held -- 4 messages with no bounces is not evidence", d.Decision)
	}
	if !strings.Contains(d.Reason, "empty numerator") {
		t.Errorf("the reason must say WHY a clean rate was not enough; got %q", d.Reason)
	}
}

// TestTimeAloneDoesNotAdvance: the other half of the same point.
func TestTimeAloneDoesNotAdvance(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	state := startedState(now.Add(-2*time.Hour), 0) // plenty of volume, not enough time

	d := w.evaluateWarmup(state, AggregateReputation([]ReputationWindow{rep("gmail.com", 5000, 0, 0, 0)}), now)

	if d.Decision != "held" {
		t.Fatalf("decision = %q, want held", d.Decision)
	}
	if !strings.Contains(d.Reason, "minimum") {
		t.Errorf("reason = %q, want the hold time named", d.Reason)
	}
}

func TestGoodEvidenceAdvancesTheRamp(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	state := startedState(now.Add(-30*time.Hour), 0)

	d := w.evaluateWarmup(state, AggregateReputation([]ReputationWindow{
		rep("gmail.com", 900, 5, 12, 0),
		rep("outlook.com", 300, 1, 3, 0),
	}), now)

	if d.Decision != "advanced" || d.Step != 2 {
		t.Fatalf("decision = %q step = %d, want advanced to 2", d.Decision, d.Step)
	}
	if d.RatePerMinute != 25 {
		t.Errorf("rate = %d, want the third step's 25", d.RatePerMinute)
	}
	if !d.StepEnteredAt.Equal(now) {
		t.Error("advancing must restart the step clock, or the next advance inherits this step's hold time")
	}
}

// TestOneBadDomainReducesTheRamp: a domain over threshold does not merely
// hold the ramp. Holding at a rate that is already producing complaints is
// not a neutral act.
func TestOneBadDomainReducesTheRamp(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	state := startedState(now.Add(-30*time.Hour), 0)

	d := w.evaluateWarmup(state, AggregateReputation([]ReputationWindow{
		rep("gmail.com", 1000, 0, 0, 6), // 0.6%, six times the threshold
	}), now)

	if d.Decision != "reduced" || d.Step != 0 {
		t.Fatalf("decision = %q step = %d, want a reduction to step 0", d.Decision, d.Step)
	}
	if !strings.Contains(d.Reason, "gmail.com") || !strings.Contains(d.Reason, "complaint rate") {
		t.Errorf("reason = %q, want the domain and the rate named", d.Reason)
	}
}

// TestHealthyAggregateDoesNotHideABadDomain is why the counters are per
// domain at all. In aggregate this deployment looks fine; at one provider it
// is being reported as spam, and that provider is the one that blocks.
func TestHealthyAggregateDoesNotHideABadDomain(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	state := startedState(now.Add(-30*time.Hour), 0)

	rows := []ReputationWindow{
		rep("gmail.com", 50000, 0, 0, 0),
		rep("smallco.test", 200, 0, 0, 5), // 2.5% -- ruinous, invisible in the total
	}
	if total := AggregateReputation(rows)[""]; total.ComplaintRate() > w.cfg.WarmupMaxComplaintRate {
		t.Fatal("the fixture does not exercise the case: the aggregate is over threshold too")
	}

	d := w.evaluateWarmup(state, AggregateReputation(rows), now)
	if d.Decision != "reduced" {
		t.Fatalf("decision = %q; a bad domain inside a healthy total is exactly what per-domain counters exist to catch", d.Decision)
	}
	if !strings.Contains(d.Reason, "smallco.test") {
		t.Errorf("reason = %q, want the offending domain named", d.Reason)
	}
}

// TestTinyDomainCannotPinTheRamp: one bounce at a domain we sent three
// messages to is 33%, and treating that as a verdict would stop the ramp
// forever.
func TestTinyDomainCannotPinTheRamp(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	state := startedState(now.Add(-30*time.Hour), 0)

	d := w.evaluateWarmup(state, AggregateReputation([]ReputationWindow{
		rep("gmail.com", 900, 2, 0, 0),
		rep("tiny.test", 3, 1, 0, 0), // 33%, on three messages
	}), now)

	if d.Decision != "advanced" {
		t.Fatalf("decision = %q; a domain below the minimum volume must not hold the ramp: %s", d.Decision, d.Reason)
	}
}

// TestRampNeverRaisesPastTheConfiguredRate: the ramp may hold the operator's
// rate down and may never raise it. That direction is the safety property.
func TestRampNeverRaisesPastTheConfiguredRate(t *testing.T) {
	w := warmupWorker(t)
	w.cfg.SendRatePerMinute = 12
	w.limiter = newRateLimiter(w.cfg.SendRatePerMinute, w.now)

	w.setWarmupRate(warmupDecision{Step: 3, RatePerMinute: 50})
	if got := w.limiter.Rate(); got != 12 {
		t.Errorf("the ramp raised the send rate to %d past the configured 12", got)
	}

	w.setWarmupRate(warmupDecision{Step: 0, RatePerMinute: 5})
	if got := w.limiter.Rate(); got != 5 {
		t.Errorf("the ramp did not slow the rate: %d", got)
	}
}

// TestEveryDecisionCarriesItsReason: the issue asks for operator visibility
// of "the current step, why it last advanced or held". An automated pacer
// whose decisions cannot be read is one an operator has to trust rather than
// check.
func TestEveryDecisionCarriesItsReason(t *testing.T) {
	w := warmupWorker(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		state WarmupState
		rows  []ReputationWindow
	}{
		"started":  {WarmupState{}, nil},
		"held":     {startedState(now.Add(-time.Hour), 0), []ReputationWindow{rep("gmail.com", 900, 0, 0, 0)}},
		"advanced": {startedState(now.Add(-30*time.Hour), 0), []ReputationWindow{rep("gmail.com", 900, 0, 0, 0)}},
		"reduced":  {startedState(now.Add(-30*time.Hour), 0), []ReputationWindow{rep("gmail.com", 900, 90, 0, 0)}},
	}
	for want, tc := range cases {
		d := w.evaluateWarmup(tc.state, AggregateReputation(tc.rows), now)
		if d.Decision != want {
			t.Errorf("%s: decision = %q", want, d.Decision)
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s: no reason recorded", want)
		}
	}
}

// TestMalformedLadderLeavesTheRampOff: a pacer that silently picks its own
// rates from a list it could not read is worse than one that does not run.
func TestMalformedLadderLeavesTheRampOff(t *testing.T) {
	if _, err := parseWarmupSteps("5,10,7,50"); err == nil {
		t.Error("a descending step was accepted; sorting it would run a ladder the operator did not write")
	}
	if _, err := parseWarmupSteps("5,ten,25"); err == nil {
		t.Error("a non-numeric step was accepted")
	}
	steps, err := parseWarmupSteps(" 5 , 10,25 ")
	if err != nil || len(steps) != 3 || steps[2] != 25 {
		t.Errorf("a well-formed ladder was rejected: %v %v", steps, err)
	}
}

// TestDisabledRampDoesNotTouchTheRate is the default, and the reason it is
// the default: an established sending domain does not want its rate
// re-derived by a control loop that has never seen it.
func TestDisabledRampDoesNotTouchTheRate(t *testing.T) {
	w := newTestWorker(t, &fakeEngine{}, &recordingSender{})
	before := w.limiter.Rate()

	w.applyWarmup(w.systemActorContext(t.Context()), time.Now())

	if after := w.limiter.Rate(); after != before {
		t.Errorf("the disabled ramp changed the send rate from %d to %d", before, after)
	}
}

// TestReputationSurvivesARestart: counters are written as absolute totals to
// this replica's own row, so a process that restarted mid-day must seed from
// what it already wrote. Without it a rolling deploy quietly resets the
// evidence the ramp is holding on.
func TestReputationSurvivesARestart(t *testing.T) {
	c := newReputationCollector("sender@example.test", "n1")
	day := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	c.observe(day, "a@gmail.com", "accepted")

	key := reputationKey{identity: "sender@example.test", domain: "gmail.com", day: "2026-08-10"}
	c.seed(key, reputationCounts{accepted: 500, hardBounce: 3})

	counts, ok := c.snapshot(key)
	if !ok || counts.accepted != 501 || counts.hardBounce != 3 {
		t.Fatalf("seeding did not fold the earlier total back in: %+v", counts)
	}

	// Seeding is once per bucket per process; a second seed would double it.
	c.seed(key, reputationCounts{accepted: 500})
	if counts, _ := c.snapshot(key); counts.accepted != 501 {
		t.Errorf("the bucket was seeded twice: accepted = %d", counts.accepted)
	}
}
