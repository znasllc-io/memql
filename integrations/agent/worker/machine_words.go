package worker

// machine_words.go -- how the dispatcher names a machine in words a public
// check run may print.
//
// A pipeline step's result and the refusals around it travel into a check run:
// the fleet executor copies the result's machine into the attempt line and
// quotes a refusal's text whole into a failed step's message, and the check run
// is public on GitHub for a public repository. Candidate.Label() is right for
// the owner's own screens and for logs, but it falls back to the name the
// machine's Cockpit reported, which is its hostname unless the owner replaced
// it ("Joses-MacBook-Pro"). Nobody chose to publish that.
//
// So there are two names, and the choice is made here and nowhere else:
//
//   - ownerChosenName is the display name the owner set in Fleet, or "". It is
//     what Result.MachineName carries.
//   - checkRunName is that name or, for a machine its owner never named, the
//     start of its registration id (pipelines.MachineWords, which the check run's
//     own attempt lines use too, so a machine reads alike in both places).
//
// Every sentence of a pipeline dispatch that names a machine -- a refusal the
// dispatcher words (dispatch.go) and a reason the router gives for turning a
// machine away (router.go) -- uses checkRunName. Candidate.Label() stays for log
// lines and for the screens only the owner sees (the consent card, the machine
// chooser, availability, model and app calls), none of which a pipeline step's
// message can quote.
//
// What this does not cover: words the machine itself composes (its Cockpit's
// refusal text) are quoted as they arrive, and the dispatcher cannot know
// whether one names its host.
//
// Covered by TestARefusalNeverNamesAMachineByWhatItReported and
// TestAMachineIsNamedOnlyByTheNameItsOwnerChose.

import (
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// ownerChosenName is the display name the machine's owner gave it in Fleet,
// trimmed, and "" when it has none. It is never the name the machine reported.
func ownerChosenName(c Candidate) string {
	return strings.TrimSpace(c.DisplayName)
}

// checkRunName is how a sentence that can reach a check run names the machine:
// the owner's display name, else "machine " and the start of its registration id.
func checkRunName(c Candidate) string {
	return pl.MachineWords(ownerChosenName(c), c.RegistrationId)
}
