package router

// failure_reason.go -- what a decision row says about a source that was
// SELECTED and then failed when it was called.
//
// THE REASON GOES IN `considered`, BECAUSE THAT IS WHAT THE DECISIONS LIST
// READS. routerDecision (dsl/router/shapes.memql) projects the walk and
// deliberately not the error message, which is the field most likely to carry
// something a reader of that list should not see. So a failed attempt's
// errorMessage was readable nowhere a person looks, and its `considered` line
// still said "selected": Settings > Decisions showed a call that failed
// through a signed-in app -- or went on to a paid vendor -- with no reason.
//
// THE LINE IS A STABLE CODE AND THE ROUTER'S OWN WORDS, NEVER THE ERROR'S
// TEXT. A refusal's message can name a person (the app gate's pin refusal
// names both the pinner and the owner) and a vendor's error can quote
// whatever the vendor sent back. The code is the contract every typed refusal
// already exposes (Code()); an untyped error is named by the category the
// ledger already derives (CategorizeError). The full text stays on the row's
// errorMessage, where it always was.

import (
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// failedWhenCalledPrefix starts every failed-attempt line, so a reader of the
// walk -- and a test -- can tell a failure from a door passed over.
const failedWhenCalledPrefix = "failed when called: "

// failureWords is what each stable code means, in the decision record's
// words. A code with no entry is printed alone: a code is still an answer.
//
// The app gate's codes are the agent's (integrations/agent/worker/app_gate.go,
// which pins that every one of them has words here); the rest are the
// engine's typed refusals and the ledger's error categories.
var failureWords = map[string]string{
	"app_no_owner":                    memql.AppNoOwnerReason,
	"kill_switch_engaged":             "computer use is switched off for the person the call acts for",
	"kill_switch_unreadable":          "the computer-use switch of the person the call acts for could not be read",
	"app_not_named_by_owner":          "the app was pinned by someone other than the person the call acts for",
	memql.AppRefusalCode:              "the app could not answer",
	memql.AppVisionStagingRefusalCode: "the images could not be placed in the session workspace",
	memql.RefusalCodeNoLocalModel:     "no machine could serve the model",
	"rate_limit":                      "rate limited",
	"timeout":                         "timed out",
	"auth":                            "the credentials were refused",
	"upstream":                        "the provider failed",
}

// FailureReason is the `considered` line for a source that failed when it was
// called: "failed when called: <code>: <words>".
func FailureReason(err error) string {
	code := failureCode(err)
	if words := failureWords[code]; words != "" {
		return failedWhenCalledPrefix + code + ": " + words
	}
	return failedWhenCalledPrefix + code
}

// failureCode is the most specific stable code the error carries.
//
// A no-owner refusal is asked first: AppUnavailable's own Code() is the
// generic no_app_available, and "nobody to act for" is the fact a reader
// needs. Then any typed refusal's Code(), found under whatever a door wrapped
// in front of it. Then the app door's sentinel, which every app failure
// without a code of its own carries (a session that started and failed).
// Last, the ledger's category of an untyped error.
func failureCode(err error) string {
	var unavailable *memql.AppUnavailable
	if errors.As(err, &unavailable) && unavailable.NoOwner {
		return "app_no_owner"
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		if code := strings.TrimSpace(coded.Code()); code != "" {
			return code
		}
	}
	if errors.Is(err, memql.ErrAppUnavailable) {
		return memql.AppRefusalCode
	}
	return CategorizeError(err)
}

// withFailureNoted returns considered with the line for entry replaced by the
// failure, or the failure appended when the walk never named the entry. It
// COPIES: a decision's considered slice is shared by every attempt a wrapper
// makes, and by concurrent calls on the same wrapper.
//
// The LAST line naming the entry is the attempt's own ("selected", or
// "selected from fallback chain"), which is always appended after the lines
// for the entries the walk passed over.
func withFailureNoted(considered []airoute.ConsideredEntry, entry, door string, err error) []airoute.ConsideredEntry {
	out := append([]airoute.ConsideredEntry(nil), considered...)
	line := FailureReason(err)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Entry == entry {
			out[i].Reason = line
			if out[i].Door == "" {
				out[i].Door = door
			}
			return out
		}
	}
	return append(out, airoute.ConsideredEntry{Entry: entry, Door: door, Reason: line})
}

// withAttemptFailed is the selection a wrapper carries to its NEXT attempt
// once `failed` has failed: the same decision, with the failure on the failed
// source's line. Every later row of the walk -- the served one included --
// then says the source in front of it failed, and why.
func (selection Resolved) withAttemptFailed(failed Resolved, err error) Resolved {
	selection.Decision.Considered = withFailureNoted(selection.Decision.Considered, failed.ProviderName, failed.Decision.Door, err)
	return selection
}
