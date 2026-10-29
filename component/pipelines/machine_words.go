package pipelines

import (
	"strings"
	"unicode/utf8"
)

// machine_words.go -- how a sentence that can reach a check run names a machine.
//
// A check run is public on GitHub for a public repository, so the one thing it
// may call a machine is a name the machine's OWNER chose: the display name set
// in Fleet. The name a machine's Cockpit reported is its hostname unless the
// owner replaced it ("Joses-MacBook-Pro"), and nobody chose to publish that. A
// machine its owner never named is called by the start of its registration id,
// the way a short hash names a commit.
//
// This is the one place that rule is written. The attempt lines of a check run
// (pipelinerun) and the sentences the dispatcher composes about a machine in a
// refusal, which a failed step quotes whole into its message (the worker
// package), both call it, so a machine reads the same in a step's attempt list
// and in that step's failure.
//
// Covered by TestMachineWords, and by the dispatcher's
// TestARefusalNeverNamesAMachineByWhatItReported.

// MachineIDChars is how much of a machine's registration id a sentence calls
// an unnamed machine by. Registration ids are 24 random hex characters, so the
// first eight carry 32 bits: enough to tell one owner's machines apart.
const MachineIDChars = 8

// MachineWords is how a sentence that can reach a check run names a machine:
// the display name its owner chose (ownerName, trimmed), else "machine " and
// the first MachineIDChars characters of its registration id, canonical or bare.
// With neither it is "the machine". It is never given, and so never prints, the
// name the machine reported.
func MachineWords(ownerName, registrationID string) string {
	if name := strings.TrimSpace(ownerName); name != "" {
		return name
	}
	if id := machineIDPrefix(registrationID); id != "" {
		return "machine " + id
	}
	return "the machine"
}

// machineIDPrefix is the first MachineIDChars bytes of the bare id: the text
// after the last colon of a canonical `v1:worker:registration:<id>`, cut
// without splitting a rune.
func machineIDPrefix(registrationID string) string {
	id := strings.TrimSpace(registrationID)
	id = id[strings.LastIndex(id, ":")+1:]
	n := MachineIDChars
	if len(id) <= n {
		return id
	}
	for n > 0 && !utf8.RuneStart(id[n]) {
		n--
	}
	return id[:n]
}
