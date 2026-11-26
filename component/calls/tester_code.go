package calls

import "time"

// Tester code refusals. In mock commerce mode a simulated payment or age result
// needs a tester's access code for the site's calls-testers gate; backing out
// never does.
const (
	// ReasonTesterAccessUnavailable: this site cannot hold tester codes (no package
	// deployable), so every gated simulated result is refused. Nothing is applied or
	// counted.
	ReasonTesterAccessUnavailable = "tester_access_unavailable"
	// ReasonTesterCodeRequired: a gated simulated result arrived without a code.
	ReasonTesterCodeRequired = "tester_code_required"
	// ReasonTesterCodeIncorrect: not a live calls-testers code of this site; the
	// reply says how many tries remain before the account's pause.
	ReasonTesterCodeIncorrect = "tester_code_incorrect"
	// ReasonTesterCodeExpired: exactly a tester code of this site, past its expiry.
	// It is not counted, since only the code's holder can learn this.
	ReasonTesterCodeExpired = "tester_code_expired"
	// ReasonTesterCodePaused: five wrong codes within 15 minutes paused this
	// account's simulated results; the reply says when to try again.
	ReasonTesterCodePaused = "tester_code_paused"
	// ReasonAutomaticTopupUnavailable: an agent's automatic top-up has nobody to
	// type a tester code, so it is refused while payments are simulated.
	ReasonAutomaticTopupUnavailable = "automatic_topup_unavailable"
)

// TesterCodeIncorrect is a wrong tester code with the tries that remain (1 to 4);
// the fifth wrong code answers TesterCodePaused instead.
type TesterCodeIncorrect struct{ AttemptsLeft int }

func (e *TesterCodeIncorrect) Error() string { return "calls: tester code incorrect" }
func (e *TesterCodeIncorrect) Unwrap() error {
	return Refuse(ErrForbidden, ReasonTesterCodeIncorrect, "the tester code is not correct")
}

// TesterCodePaused carries the time until the account's pause ends. A route
// answers it as a rate limit with its own reason, distinct from an address's
// generic request budget.
type TesterCodePaused struct{ RetryAfter time.Duration }

func (e *TesterCodePaused) Error() string { return "calls: tester code paused" }
func (e *TesterCodePaused) Unwrap() error {
	return Refuse(ErrConflict, ReasonTesterCodePaused, "wait before trying a tester code again")
}
