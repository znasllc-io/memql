package planner

import (
	"errors"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestProviderUnavailableIsDistinguishable is the memql#4693 core.
//
// The triage prompt names an OpenAI provider. On a Claude-only cluster the
// call cannot resolve one, and the pre-fix code could not tell that apart from
// a genuine outage -- so every goal logged a WARN that read like a fault, for a
// cluster that was configured exactly as its operator intended.
func TestProviderUnavailableIsDistinguishable(t *testing.T) {
	unavailable := memql.ErrProviderUnavailable("chat54Mini")
	if !memql.IsProviderUnavailable(unavailable) {
		t.Fatal("an unconfigured provider is not recognised as such")
	}
	// Wrapped, because the error crosses InvokeAI and the classify helper
	// before the triage path sees it.
	if !memql.IsProviderUnavailable(fmt.Errorf("invoke goalComplexityTriage: %w", unavailable)) {
		t.Error("the classification must survive wrapping; it does not reach the caller unwrapped")
	}
	// The reachable negative: a real outage must NOT be silenced, or the fix
	// has turned a loud bug into a quiet one.
	for _, other := range []error{
		errors.New("connection refused"),
		errors.New("429 rate limited"),
		fmt.Errorf("parse goalComplexityTriage output: %w", errors.New("unexpected EOF")),
	} {
		if memql.IsProviderUnavailable(other) {
			t.Errorf("%v is being treated as an unconfigured provider; a real failure would stop being reported", other)
		}
	}
}

// TestProviderUnavailableMessageIsUnchanged. The text appears in operator logs
// and in this issue; the classification is an ADDITION to it, not a rename.
func TestProviderUnavailableMessageIsUnchanged(t *testing.T) {
	if got, want := memql.ErrProviderUnavailable("chat54Mini").Error(), `provider "chat54Mini" not available`; got != want {
		t.Errorf("message changed: got %q, want %q", got, want)
	}
}

// TestTriageNoticeIsLoggedOncePerProcess pins the latch. The registry is built
// at boot and gains no entries later, so repeating the notice per goal only
// buries other logs.
func TestTriageNoticeIsLoggedOncePerProcess(t *testing.T) {
	l := &PlannerAgentLoop{}
	calls := 0
	for i := 0; i < 5; i++ {
		l.triageUnavailable.Do(func() { calls++ })
	}
	if calls != 1 {
		t.Errorf("the notice fired %d times, want exactly 1", calls)
	}
}
