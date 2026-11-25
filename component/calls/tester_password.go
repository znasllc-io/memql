package calls

import "time"

// Tester password refusals. In mock commerce mode a simulated payment or age
// result needs the installation's tester password; backing out never does.
const (
	// ReasonTesterAccessUnavailable: no valid tester password hash is configured,
	// so every gated simulated result is refused. Nothing is applied or counted.
	ReasonTesterAccessUnavailable = "tester_access_unavailable"
	// ReasonTesterPasswordRequired: a gated simulated result arrived without one.
	ReasonTesterPasswordRequired = "tester_password_required"
	// ReasonTesterPasswordIncorrect: the password did not match; the reply says
	// how many tries remain before the account's pause.
	ReasonTesterPasswordIncorrect = "tester_password_incorrect"
	// ReasonTesterPasswordPaused: five wrong passwords within 15 minutes paused
	// this account's simulated results; the reply says when to try again.
	ReasonTesterPasswordPaused = "tester_password_paused"
	// ReasonAutomaticTopupUnavailable: an agent's automatic top-up has nobody to
	// type the tester password, so it is refused while payments are simulated.
	ReasonAutomaticTopupUnavailable = "automatic_topup_unavailable"
)

// TesterPasswordIncorrect is a wrong tester password with the tries that remain
// (1 to 4); the fifth wrong password answers TesterPasswordPaused instead.
type TesterPasswordIncorrect struct{ AttemptsLeft int }

func (e *TesterPasswordIncorrect) Error() string { return "calls: tester password incorrect" }
func (e *TesterPasswordIncorrect) Unwrap() error {
	return Refuse(ErrForbidden, ReasonTesterPasswordIncorrect, "the tester password is not correct")
}

// TesterPasswordPaused carries the time until the account's pause ends. A
// route answers it as a rate limit with its own reason, distinct from an
// address's generic request budget.
type TesterPasswordPaused struct{ RetryAfter time.Duration }

func (e *TesterPasswordPaused) Error() string { return "calls: tester password paused" }
func (e *TesterPasswordPaused) Unwrap() error {
	return Refuse(ErrConflict, ReasonTesterPasswordPaused, "wait before trying the tester password again")
}
