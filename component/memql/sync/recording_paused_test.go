package sync

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A HELD DELIVERY IS TOLD FROM A FAILED ONE BY ITS STORED TEXT, compared
// exactly. So the sentinel's own text is what the runtime stores, whatever a
// connector wrapped around it: it names no connector and no tenant, it is a
// sentence a person can read, and no other failure of the contract is it.
func TestRecordingPausedIsOneSentenceThatNamesNobody(t *testing.T) {
	err := RecordingPaused("shopify", "acme-widgets")
	if !IsRecordingPaused(err) || !IsRecordingPaused(fmt.Errorf("applying: %w", err)) {
		t.Fatalf("%v is not recognised as recording paused, bare or wrapped", err)
	}
	if !strings.Contains(err.Error(), "shopify") || !strings.Contains(err.Error(), "acme-widgets") {
		t.Errorf("the error a connector returns is %q, want it to say which connector and tenant for a log", err)
	}
	text := ErrRecordingPaused.Error()
	if strings.Contains(text, "shopify") || strings.Contains(text, "acme") || strings.TrimSpace(text) != text || len(text) > 300 {
		t.Errorf("the stored text is %q: it must name nobody, carry no stray space and fit a row", text)
	}
	for _, word := range []string{"paused", "held", "resume"} {
		if !strings.Contains(text, word) {
			t.Errorf("the stored text %q does not say %q", text, word)
		}
	}
	for name, other := range map[string]error{
		"answered nothing": AnsweredNothing("shopify", "gid://shopify/Order/1"),
		"permanent":        Permanentf("pinned to another version"),
		"not granted":      NotGranted("shopify", "read_orders"),
		"not requested":    NotRequested("shopify"),
		"busy":             Busy("shopify", ""),
		"not implemented":  NotImplemented("shopify", "Apply"),
		"an ordinary one":  errors.New("origin timed out"),
	} {
		if IsRecordingPaused(other) {
			t.Errorf("%s is read as recording paused", name)
		}
	}
	if IsPermanent(err) || IsNotGranted(err) || IsAnsweredNothing(err) || IsBusy(err) {
		t.Errorf("recording paused is read as another of the contract's answers: %v", err)
	}
}

// THE HELD SENTENCE IS PINNED. A hold stores this exact text on the delivery,
// and the held read (the runtime's Store.InboundRequestsHeld) finds a held
// delivery by asking for exactly this text. Reworded, every delivery held
// under the earlier text is never found again: it stays held when recording
// resumes, with no next try, and is never counted. A change to the sentence
// must ALSO return the rows held under the earlier text -- read them by the
// old sentence and put them back in line, or restamp them with the new one --
// in the same change, and then move this literal.
func TestTheHeldSentenceIsPinned(t *testing.T) {
	const held = "connector: recording is paused, so this delivery is held; it goes back in line when recording resumes"
	if got := ErrRecordingPaused.Error(); got != held {
		t.Fatalf("ErrRecordingPaused reads %q, pinned as %q: the deliveries held under the pinned text must be returned before it changes", got, held)
	}
}
