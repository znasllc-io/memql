package sync

import (
	"errors"
	"fmt"
	"testing"
)

// A FAILURE THAT WAITS ITS CADENCE SAYS ONLY WHAT IT SAYS. The runtime stores
// its text on the domain's row, where a person reads it as the detail of a
// failed check, so the mark adds no words; and it is none of the contract's
// other answers.
func TestAFailureThatWaitsItsCadenceKeepsItsOwnSentence(t *testing.T) {
	const sentence = "shopify: 1 row read from the store could not be written to the mirror; the next check reads it again"
	err := WaitsCadence(errors.New(sentence))
	if got := err.Error(); got != sentence {
		t.Errorf("the failure reads %q, want exactly %q", got, sentence)
	}
	if !IsWaitingCadence(err) || !IsWaitingCadence(fmt.Errorf("datasync: reconciling: %w", err)) {
		t.Errorf("%v is not read as waiting its cadence, bare or wrapped", err)
	}
	if IsPermanent(err) || IsNotGranted(err) || IsNotRequested(err) || IsBusy(err) || IsAnsweredNothing(err) || IsRecordingPaused(err) || IsNotImplemented(err) {
		t.Errorf("a failure that waits its cadence is read as another of the contract's answers: %v", err)
	}
	if WaitsCadence(nil) != nil {
		t.Error("no failure was marked as one")
	}
	if IsWaitingCadence(errors.New(sentence)) || IsWaitingCadence(Permanent(errors.New(sentence))) || IsWaitingCadence(nil) {
		t.Error("an unmarked failure is read as waiting its cadence")
	}
}
