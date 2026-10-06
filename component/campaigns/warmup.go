package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"strings"
	"time"
)

// warmup.go -- the ramp, and it advances on evidence (memql#3462).
//
// # Why there was no ramp before, and what changed
//
// Warming is raising send volume gradually on a new sending identity, so a
// mailbox provider sees a growing trickle rather than a cold blast. The
// procedure has been in the runbook since memql#3348 and nothing scheduled
// it, deliberately: A FIXED-SCHEDULE RAMP IS A GUESS WITH A SCHEDULE
// ATTACHED. Real warming is feedback-driven -- you raise volume when the
// previous step produced good signals and hold or back off when it did not
// -- and this deployment collected none of those signals. Building the ramp
// anyway would have produced the worst kind of false confidence: an operator
// believing they were warming safely while the system had no idea whether it
// was working.
//
// memql#3461 supplied the feedback and reputation.go supplies the counters,
// so the ramp can now be what it should be: a control loop that reads
// evidence, with the evidence readable next to it.
//
// # Off by default, and it can only ever slow you down
//
// MEMQL_CAMPAIGNS_WARMUP_ENABLED defaults to false. An existing deployment
// with an established sending domain does not want its rate re-derived from
// a ramp that has never seen it, and a control loop nobody asked for
// deciding how fast their mail goes out is a bad surprise.
//
// When on, the effective rate is min(step rate, MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE).
// The ramp cannot raise the rate past what the operator configured; it can
// only hold it lower while the evidence is thin. That direction is the whole
// safety property.
//
// # The advance condition, and why each part is there
//
// A step advances only when ALL of these hold:
//
//	held long enough   MEMQL_CAMPAIGNS_WARMUP_MIN_HOURS_PER_STEP. Providers
//	                   judge over time; a step that ran for ten minutes has
//	                   not been seen yet, however clean it looks.
//	sent enough        MEMQL_CAMPAIGNS_WARMUP_MIN_VOLUME_PER_STEP. A step
//	                   that sent four messages with no bounces has a hard
//	                   bounce rate of 0.0 and has proved nothing. This is
//	                   the condition that stops an empty numerator reading
//	                   as a clean bill of health.
//	rates within bound PER DOMAIN, not in aggregate. One badly-performing
//	                   domain inside a large healthy total is exactly what an
//	                   aggregate hides, and it is the shape that gets a
//	                   sending domain blocked.
//
// A domain over threshold does not merely hold the ramp -- it REDUCES it,
// back to the previous step. Holding at a rate that is already producing
// complaints is not a neutral act.
//
// # Domains too small to judge are ignored, and that is a stated limitation
//
// A domain with fewer than the minimum volume of its own contributes no
// verdict either way. Otherwise a single bounce at a domain we sent three
// messages to (33%) would pin the ramp forever. The cost is that a genuinely
// bad small domain is invisible to the ramp until it grows, which is a real
// gap rather than a hidden one.

const (
	// warmupEvalInterval bounds how often the ramp re-reads the evidence.
	// The decision changes on the scale of a day; re-deriving it every
	// 15-second drain tick would be a query per tick for nothing.
	warmupEvalInterval = 5 * time.Minute

	// warmupWindowDays is how much history a decision looks at. Long enough
	// that a step's own volume is inside it, short enough that a problem
	// fixed last week stops holding the ramp.
	warmupWindowDays = 7
)

// warmupDecision is what one evaluation concluded.
type warmupDecision struct {
	Step           int
	RatePerMinute  int
	Decision       string // advanced | held | reduced | started
	Reason         string
	StepEnteredAt  time.Time
	AcceptedInStep int
}

// applyWarmup re-evaluates the ramp if it is enabled and due, and sets the
// limiter accordingly. Called once per drain pass.
//
// # One ladder per identity, one rate per process (memql#4821)
//
// `warmupStateForIdentity` was built for plurality and, until sending
// identities existed, received exactly one member: every counter row carried
// the deployment's single label, so a per-identity read read a singular
// world. It now evaluates ONE LADDER PER IDENTITY this process has actually
// mailed as, plus the configured default -- so a deployment sending three
// clients' campaigns from three mailboxes warms three ladders and each one
// advances on its own evidence.
//
// The LIMITER, though, is one bucket for the whole process, and there is no
// honest way to make it three: the token bucket paces this replica's outbound
// calls, not a mailbox's. So the strictest step wins. That direction is
// forced rather than chosen -- taking the highest would send a cold mailbox's
// mail at a warm one's rate, which is the exact failure warming exists to
// prevent, and the ramp's standing invariant is that it may only ever hold
// the rate DOWN.
//
// The cost is stated rather than hidden: a warm identity sharing a process
// with a cold one sends slower than its own ladder allows. That is a
// throughput cost on a marketing message, against a reputation cost on a new
// domain, and only one of those is recoverable.
func (w *Worker) applyWarmup(systemCtx context.Context, now time.Time) {
	if !w.cfg.WarmupEnabled || len(w.cfg.WarmupSteps) == 0 || w.store == nil {
		return
	}
	w.mu.Lock()
	due := w.warmupEvaluatedAt.IsZero() || now.Sub(w.warmupEvaluatedAt) >= warmupEvalInterval
	if due {
		w.warmupEvaluatedAt = now
	}
	w.mu.Unlock()
	if !due {
		return
	}

	// ONE reputation read for every identity. The rows are keyed by
	// (identity, domain, day, node) and AggregateReputation folds by domain,
	// so the aggregate is sliced per identity here rather than re-queried --
	// a query per identity would multiply the ramp's cost by the number of
	// mailboxes for evidence already in hand.
	rows, err := w.store.ReputationSince(systemCtx, now.AddDate(0, 0, -warmupWindowDays).UTC().Format(time.DateOnly))
	if err != nil {
		w.logger.Debug("campaigns: could not read reputation counters", "error", err)
		return
	}

	slowest := 0
	for _, identity := range w.warmupIdentities() {
		state, _, err := w.store.WarmupState(systemCtx, identity)
		if err != nil {
			w.logger.Debug("campaigns: could not read the warming ramp state (engine likely not ready)",
				"identity", identity, "error", err)
			continue
		}
		next := w.evaluateWarmup(state, AggregateReputation(rowsForIdentity(rows, identity)), now)
		if err := w.store.RecordWarmupState(systemCtx, identity, next); err != nil {
			w.logger.Warn("campaigns: could not record the warming ramp decision", "identity", identity, "error", err)
		}
		if rate := next.RatePerMinute; rate > 0 && (slowest == 0 || rate < slowest) {
			slowest = rate
		}
		if next.Decision != "held" {
			w.logger.Info("campaigns: warming ramp "+next.Decision,
				"identity", identity, "step", next.Step,
				"ratePerMinute", next.RatePerMinute, "reason", next.Reason)
		}
	}
	w.setWarmupRate(warmupDecision{RatePerMinute: slowest})
}

// warmupIdentities is every ladder this replica evaluates: the configured
// default, plus each identity it has actually mailed as.
//
// The default is ALWAYS included, even on a process that has only ever sent
// as named identities. Dropping it would mean a deployment's own mailbox
// silently stops being warmed the moment the first client identity is added
// -- and warming state that stops advancing looks identical to warming state
// that is holding on evidence.
func (w *Worker) warmupIdentities() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []string{w.cfg.SendingIdentityFor("")}
	for identity := range w.activeIdentities {
		if identity != out[0] {
			out = append(out, identity)
		}
	}
	sortStrings(out)
	return out
}

// rowsForIdentity narrows the reputation window rows to one sending identity.
//
// A row whose sendingIdentity is EMPTY belongs to the default ladder, because
// that is what an unlabelled counter meant before identities existed and
// re-attributing that history to nothing would erase every deployment's
// warming evidence at the moment it first declares an identity.
func rowsForIdentity(rows []ReputationWindow, identity string) []ReputationWindow {
	out := make([]ReputationWindow, 0, len(rows))
	for _, r := range rows {
		rowIdentity := strings.TrimSpace(r.SendingIdentity)
		if rowIdentity == identity || (rowIdentity == "" && identity == "default") {
			out = append(out, r)
		}
	}
	return out
}

// setWarmupRate applies the step to the send limiter. min(), never max():
// the ramp may hold the operator's configured rate down and may never raise
// it.
func (w *Worker) setWarmupRate(d warmupDecision) {
	rate := d.RatePerMinute
	if rate <= 0 || rate > w.cfg.SendRatePerMinute {
		rate = w.cfg.SendRatePerMinute
	}
	if w.limiter != nil {
		w.limiter.SetRate(rate)
	}
}

// evaluateWarmup is the decision, isolated from the engine so it is
// provable without one.
func (w *Worker) evaluateWarmup(state WarmupState, reputation map[string]DomainReputation, now time.Time) warmupDecision {
	steps := w.cfg.WarmupSteps
	if len(steps) == 0 {
		return warmupDecision{Decision: "held", Reason: "No warmup rates are configured."}
	}
	step := state.Step
	if step < 0 || step >= len(steps) {
		step = 0
	}
	accepted := max(reputation[""].Accepted-state.AcceptedAtStepStart, 0)
	held := now.Sub(state.StepEnteredAt)
	args := w.warmupFacts(reputation)
	for k, v := range map[string]any{"step": step, "stepLabel": fmt.Sprint(step + 1), "stepCount": fmt.Sprint(len(steps)), "finalStep": len(steps) - 1, "rateLabel": fmt.Sprint(steps[step]),
		"started": !state.StepEnteredAt.IsZero(), "accepted": accepted, "acceptedLabel": fmt.Sprint(accepted), "minVolume": w.cfg.WarmupMinVolumePerStep, "minVolumeLabel": fmt.Sprint(w.cfg.WarmupMinVolumePerStep),
		"heldHours": held.Hours(), "minHours": w.cfg.WarmupMinHoursPerStep.Hours(), "minDurationLabel": w.cfg.WarmupMinHoursPerStep.String(), "heldMinutesLabel": held.Round(time.Minute).String(), "heldHoursLabel": held.Round(time.Hour).String()} {
		args[k] = v
	}
	value, err := workflowhost.Run(context.Background(), "campaignWarmupDecision", args, workflowhost.Options{})
	var choice struct {
		Step     int
		Decision string
		Reason   string
		Reset    bool
	}
	if err == nil {
		encoded, e := json.Marshal(value)
		err = e
		if err == nil {
			err = json.Unmarshal(encoded, &choice)
		}
	}
	if err != nil || choice.Step < 0 || choice.Step >= len(steps) {
		if w.logger != nil {
			w.logger.Error("campaign warmup workflow failed; holding the lowest rate", "error", err)
		}
		return warmupDecision{Step: 0, RatePerMinute: steps[0], Decision: "held", Reason: "Warmup policy could not be evaluated; holding the lowest rate.", StepEnteredAt: state.StepEnteredAt, AcceptedInStep: accepted}
	}
	out := warmupDecision{Step: choice.Step, RatePerMinute: steps[choice.Step], Decision: choice.Decision, Reason: choice.Reason, StepEnteredAt: state.StepEnteredAt, AcceptedInStep: accepted}
	if choice.Reset {
		out.StepEnteredAt = now
		out.AcceptedInStep = 0
	}
	return out
}

func (w *Worker) warmupFacts(reputation map[string]DomainReputation) map[string]any {
	names := []string{}
	for name := range reputation {
		if name != "" {
			names = append(names, name)
		}
	}
	sortStrings(names)
	domains := []any{}
	for _, name := range names {
		d := reputation[name]
		domains = append(domains, map[string]any{"name": name, "accepted": d.Accepted, "acceptedLabel": fmt.Sprint(d.Accepted), "complaintsLabel": fmt.Sprint(d.Complaint), "bouncesLabel": fmt.Sprint(d.HardBounce),
			"complaintRate": d.ComplaintRate(), "bounceRate": d.HardBounceRate(), "complaintLabel": fmt.Sprintf("%.3f", d.ComplaintRate()*100), "bounceLabel": fmt.Sprintf("%.2f", d.HardBounceRate()*100)})
	}
	return map[string]any{"domains": domains, "minDomainVolume": w.cfg.WarmupMinDomainVolume, "maxComplaintRate": w.cfg.WarmupMaxComplaintRate, "maxBounceRate": w.cfg.WarmupMaxHardBounceRate,
		"maxComplaintLabel": fmt.Sprintf("%.3f", w.cfg.WarmupMaxComplaintRate*100), "maxBounceLabel": fmt.Sprintf("%.2f", w.cfg.WarmupMaxHardBounceRate*100)}
}

// worstDomain returns the first domain over a threshold, and why.
//
// Per domain, and only for domains with enough volume of their OWN to
// judge -- otherwise one bounce at a domain we sent three messages to reads
// as 33% and pins the ramp permanently. The stated cost is that a genuinely
// bad small domain is invisible here until it grows.
func (w *Worker) worstDomain(reputation map[string]DomainReputation) (string, string) {
	value, err := workflowhost.Run(context.Background(), "campaignWarmupWorstDomain", w.warmupFacts(reputation), workflowhost.Options{})
	if err != nil {
		return "", "Warmup policy could not be evaluated"
	}
	result, _ := value.(map[string]any)
	name, _ := result["domain"].(string)
	reason, _ := result["reason"].(string)
	return name, reason
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// parseWarmupSteps reads `5,10,25,50` into an ascending ladder.
//
// A non-ascending ladder is REFUSED rather than sorted, because sorting it
// would silently run a ramp the operator did not write -- and the value they
// typed is the one they will read back when they ask why the rate is what it
// is. An unparseable list leaves the ramp off, which is the same state as not
// configuring it.
func parseWarmupSteps(raw string) ([]int, error) {
	fields := strings.Split(raw, ",")
	steps := make([]int, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(f, "%d", &n); err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a positive messages-per-minute value", f)
		}
		if len(steps) > 0 && n <= steps[len(steps)-1] {
			return nil, fmt.Errorf("step %d is not greater than the step before it (%d); a ramp has to ascend, and sorting it for you would run a ladder you did not write", n, steps[len(steps)-1])
		}
		steps = append(steps, n)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no steps")
	}
	return steps, nil
}
