package sync

import (
	"errors"
	"fmt"
	"testing"
)

// A BUSY ANSWER SAYS WHY, IN THE CONNECTOR'S OWN WORDS. The runtime treats
// every busy answer the same way -- nothing is recorded as an error and the
// work is asked again -- but it is not always the same thing: the origin
// running another export, or a store that does not ingest now. Whoever asked
// is told which, so the reason is carried apart from the sentence around it.
func TestABusyAnswerCarriesItsReason(t *testing.T) {
	const why = `store "acme" does not ingest now, so its mirror waits for it`
	with := Busy("shopify", why)
	if got := BusyReason(with); got != why {
		t.Errorf("BusyReason = %q, want the connector's own reason %q", got, why)
	}
	if got := BusyReason(fmt.Errorf("datasync: backfilling: %w", with)); got != why {
		t.Errorf("wrapped, BusyReason = %q, want %q", got, why)
	}
	without := Busy("shopify", "  ")
	if got := BusyReason(without); got != "" {
		t.Errorf("a busy answer that gave no reason carries %q", got)
	}
	if got := BusyReason(errors.New("connector: busy: shopify: said by something else")); got != "" {
		t.Errorf("an error that only reads like a busy answer carries %q", got)
	}
	if got := BusyReason(nil); got != "" {
		t.Errorf("no error carries %q", got)
	}
	// What it is and what it says are as they were.
	for _, err := range []error{with, without} {
		if !IsBusy(err) || IsPermanent(err) || IsNotGranted(err) {
			t.Errorf("%v: busy %t, permanent %t, not granted %t; want busy and nothing else", err, IsBusy(err), IsPermanent(err), IsNotGranted(err))
		}
	}
	if got, want := with.Error(), "connector: busy: shopify: "+why; got != want {
		t.Errorf("the answer reads %q, want %q", got, want)
	}
	if got, want := without.Error(), "connector: busy: shopify is busy with another operation"; got != want {
		t.Errorf("the answer with no reason reads %q, want %q", got, want)
	}
}
