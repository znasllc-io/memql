// Package apps knows which local coding apps this machine has, whether
// they can actually be driven, and how to invoke them.
//
// It exists because the engine cannot discover any of this. The cockpit
// dials out from behind NAT; the engine reads what the cockpit reports on
// Register and every Heartbeat and derives `app:<id>` routing labels from
// it (memql#4359). Nothing here decides whether a run happens -- that is
// the engine's delegation policy plus the consent gates -- but everything
// here decides whether this machine is ELIGIBLE to be picked.
//
// Two rules run through the whole package:
//
//   - `signed_in=false` beats a guess. A label is derived only from an
//     entry that is BOTH allowed and signed in, precisely so the router
//     cannot select a machine that would then refuse the run. A
//     best-effort `true` produces a plan committed to a laptop that says
//     no, and the resulting failure names the router rather than the auth
//     state. So every probe here reports false when it cannot tell.
//
//   - `subscription` is REPORTED, never inferred. The closed set is
//     unknown / none / present, and "unknown" is the normal answer today.
//     The engine records unknown as billing "unknown", which is honest; a
//     value derived from "well, they have the binary" would be recorded
//     as measured.
//
// The engine's runnable set is closed (claude-code, codex). A cockpit may
// report more -- they are stored and never driven -- but there is no
// reason for this package to invent any.
package apps

import (
	"strings"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// The app ids the engine can drive. Closed set, mirrored from
// component/worker/apps.go in the engine repo.
const (
	IDClaudeCode = "claude-code"
	IDCodex      = "codex"
)

// The closed set of subscription answers. Mirrored from the engine's
// NormalizeSubscription, which folds anything else -- empty included --
// to Unknown.
const (
	SubscriptionUnknown = "unknown"
	SubscriptionNone    = "none"
	SubscriptionPresent = "present"
)

// The harness words: which protocol the cockpit drives an app through.
//
// They are mirrored by hand from internal/worker/harness rather than
// imported. This package is the one that puts the word on the wire, and
// an inventory that only compiles when the process drivers do stops being
// reportable exactly when something is wrong with them.
// TestHarnessWordsMirrorTheWire pins the spelling; a rename on one side
// alone compiles everywhere and fails at the only moment that matters --
// the session's Start, with "no client for harness <word>".
//
// TWO OF THEM ARE CODEX, and that is the reason the engine must READ this
// word rather than infer it from the app id. A Codex old enough to lack
// `app-server` is still perfectly drivable through `codex mcp-server`,
// with a thread id in place of usage; a Codex that has the app-server
// gives up structured results and real accounting. Nothing off this
// machine can tell which one is installed here, so a descriptor derived
// from the id would be right on roughly half the fleet and would fail on
// the other half only after a turn had been committed to it.
// The words are ALIASED from internal/worker/harness rather than
// re-spelled here, and that is the one place this package does not follow
// its own mirroring convention. Mirroring exists for strings that live in
// the ENGINE repo, where importing is not possible; harness is in this
// module, so a copy here would be two definitions that a rename can pull
// apart in silence -- the detector would report a word, the runner would
// answer to a different one, and every session for that app would fail at
// Start with "no client for harness".
const (
	// HarnessClaudeHeadless is `claude -p` with --resume, one process
	// per turn.
	HarnessClaudeHeadless = harness.HarnessClaudeHeadless
	// HarnessCodexAppServer is `codex app-server`, JSON-RPC over stdio.
	HarnessCodexAppServer = harness.HarnessCodexAppServer
	// HarnessCodexMCP is `codex mcp-server`, the codex / codex-reply
	// tool pair over stdio MCP. The fallback, and the floor: a Codex
	// that cannot be driven this way cannot be driven at all.
	HarnessCodexMCP = harness.HarnessCodexMCP
)

// MaxFieldLen bounds each reported string. The engine truncates at 200
// on its side; matching it here means an operator reading the portal sees
// the same value the cockpit logged, rather than a longer one that got
// clipped in transit.
const MaxFieldLen = 200

// Info is one detected app, in the shape Register and Heartbeat carry.
//
// Allowed and SignedIn are separate on purpose and both must be true
// before the engine will route to this machine. Allowed is this machine's
// own policy.yaml verdict; SignedIn is the app's auth state as the
// cockpit can observe it without spending the user's tokens to ask.
//
// THE DESCRIPTOR RIDES REGISTER, not AppInfo. Register carries it as
// app_descriptors (engine epic memql#5096), one per app, and
// appinventory.go's appDescriptorsToProto maps the three fields below into
// it. The pin carried that field from 2026-09-08 and nothing set it until
// memql-cockpit#444 -- an absent descriptor reads as "assume capable" on
// the engine side, so the gap cost structured answers on every machine
// that could not give one. The app-session runner reads the same answer
// here through ResolveSpec, so the protocol a session drives and the one
// the registration advertised cannot be a probe apart.
type Info struct {
	Id           string
	Version      string
	SignedIn     bool
	Subscription string
	Allowed      bool
	// Harness is the protocol word for the harness that can drive this
	// app ON THIS MACHINE, resolved against the installed binary rather
	// than assumed from the id.
	Harness string
	// StructuredResult reports whether that harness can return a final
	// answer against a schema. False is the fail-closed answer: the
	// engine simply does not send this machine a structured call, which
	// costs a door. A false TRUE costs a parse failure in the engine's
	// structured path three layers away, naming nothing here.
	StructuredResult bool
	// FollowUps reports whether that harness can send a second prompt
	// into the session it already opened. False means every turn is a
	// fresh conversation, which the engine can work with; claiming it
	// falsely turns a follow-up into a stranger with no context.
	FollowUps bool
}

// Spec is everything the cockpit needs to detect and drive one app.
//
// It is data rather than an interface because the set is closed and small;
// a plugin seam here would be a seam for apps the engine cannot drive.
type Spec struct {
	// ID is the engine's app id.
	ID string
	// Binary is what the app calls itself on PATH -- and, on a spec
	// Detector.ResolveSpec returned, the path that lookup resolved it to.
	Binary string
	// VersionArgs asks the app for its own version. Whatever it prints
	// is reported verbatim -- the engine reduces it to major.minor for
	// the label, so pre-reducing it here would throw away the patch
	// level the portal shows.
	VersionArgs []string
	// StreamsJSON USED TO BE HERE. It answered one question -- "may the
	// runner try to parse this app's stdout as newline-delimited JSON?"
	// -- for a classifier that no longer exists: the harness reads each
	// app's own protocol and labels every chunk itself. A flag kept
	// past its only reader is a flag the next person has to work out
	// the meaning of before they can ignore it.
	//
	// Harness is the protocol word the app-session runner drives this
	// app through. What Specs() carries is the FLOOR -- the harness that
	// works on every machine that has the binary at all. A machine whose
	// own binary offers a better one earns it through harnessUpgrade;
	// putting the better one here instead would have a cockpit advertise
	// a protocol nothing on that machine speaks.
	Harness string
	// StructuredResult reports whether Harness can return a final answer
	// against a schema, and FollowUps whether it can send a second
	// prompt into the session it already opened. Both belong to the
	// HARNESS rather than to the app: the two Codex harnesses do not
	// answer them the same way, so they move together with the word.
	StructuredResult bool
	FollowUps        bool
}

// Specs returns the closed set, in stable id order.
func Specs() []Spec {
	return []Spec{
		{
			ID:     IDClaudeCode,
			Binary: "claude",
			// `claude --version` prints e.g. "2.1.4 (Claude Code)".
			VersionArgs: []string{"--version"},
			// Claude Code has exactly one protocol, so there is nothing
			// to probe: `-p --output-format stream-json` with
			// `--json-schema` for the answer and `--resume` for the next
			// turn. Probing for it would fork a subprocess on every beat
			// to re-read what is written here.
			Harness:          HarnessClaudeHeadless,
			StructuredResult: true,
			FollowUps:        true,
		},
		{
			ID:     IDCodex,
			Binary: "codex",
			// `codex --version` prints e.g. "codex-cli 0.9.1".
			VersionArgs: []string{"--version"},
			// The FLOOR, not the preference. Every Codex has the
			// mcp-server tool pair; only a recent one has the
			// app-server, and harnessUpgrade is what finds out.
			// StructuredResult is false here because the tool pair
			// returns a transcript rather than an answer against a
			// schema -- the engine reads the absence as "do not send
			// this machine a structured call", which is a shut door and
			// recoverable, where the over-claim is a parse failure it
			// cannot attribute to anything.
			Harness:          HarnessCodexMCP,
			StructuredResult: false,
			FollowUps:        true,
		},
	}
}

// harnessUpgrade returns the spec this app becomes when the binary on
// THIS machine answers probe successfully, and ok=false for an app whose
// harness is a constant and has nothing to ask.
//
// The upgraded word and the command that earns it live together, in this
// file, beside the floor they replace. Splitting them -- the word here
// and the probe in detect.go -- is how a probe ends up wired to the wrong
// answer, which reports a harness the binary does not have: the one
// failure this entire descriptor exists to prevent.
func (s Spec) harnessUpgrade() (upgraded Spec, probe []string, ok bool) {
	switch s.ID {
	case IDCodex:
		// `codex app-server --help` exits non-zero on a Codex that has
		// no such subcommand, and printing its help is the cheapest
		// question that distinguishes the two Codexes without starting
		// a session.
		s.Harness = HarnessCodexAppServer
		s.StructuredResult = true
		s.FollowUps = true
		return s, []string{"app-server", "--help"}, true
	}
	return s, nil, false
}

// SpecFor returns the spec for an app id.
func SpecFor(id string) (Spec, bool) {
	for _, s := range Specs() {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}

// IsKnownID reports whether id is in the engine's closed runnable set.
func IsKnownID(id string) bool {
	_, ok := SpecFor(strings.TrimSpace(id))
	return ok
}

// RunArgs and AttachArgs USED TO BE HERE, and they are gone rather than
// kept for compatibility (memql-cockpit#386).
//
// Every headless argv is now built by the harness that speaks the app's
// protocol -- internal/worker/harness -- because the argv and the parser
// that reads what comes back are one decision. Splitting them is what
// produced the bug this deletion also removes: RunArgs put the prompt
// straight after `-p`, and Claude Code's `--mcp-config` is VARIADIC, so
// a prompt following it was swallowed as a second config path; and `-p`
// is a boolean with the prompt as a trailing positional, so a prompt
// that began with a dash was read as an unknown option. Claude turns now
// send the complete prompt over text stdin, with only options in argv.
// Leaving these behind "in
// case something calls them" would have left both bugs behind with them,
// in the copy nobody was maintaining.
//
// InteractiveArgs STAYS: the `open` kind hands the app to a HUMAN with
// the prompt loaded, and that is not a harness turn.

// InteractiveArgs builds the argv that hands the app to a HUMAN with the
// prompt loaded -- the `open` kind. The workspace is the process's cwd, so
// it is not repeated in argv.
func (s Spec) InteractiveArgs(prompt string) []string {
	switch s.ID {
	case IDClaudeCode:
		if strings.TrimSpace(prompt) == "" {
			return nil
		}
		// A bare positional prompt starts an interactive session with
		// that first turn already sent.
		return []string{prompt}
	case IDCodex:
		if strings.TrimSpace(prompt) == "" {
			return nil
		}
		return []string{prompt}
	}
	return nil
}

// Truncate bounds a reported field to what the engine will store.
func Truncate(s string) string {
	if len(s) <= MaxFieldLen {
		return s
	}
	return s[:MaxFieldLen]
}

// NormalizeSubscription clamps a reported value to the closed set.
// Anything unrecognised -- empty included -- is "unknown", which is the
// honest answer rather than "none".
func NormalizeSubscription(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SubscriptionNone:
		return SubscriptionNone
	case SubscriptionPresent:
		return SubscriptionPresent
	}
	return SubscriptionUnknown
}
