package workbench

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// environment.go implements the OPTIONAL environment hint on
// workbenchDispatchHost (memql#4353, design 6.1) and the typed refusal it
// produces.
//
// The hint is the agent saying what the action NEEDS:
//
//	environment: { os: "darwin", needs: ["macos_tooling"] }
//
// A workbench is a headless Linux sandbox in the cluster with an empty
// directory tree: no display, no GPU, no macOS tooling, and none of the user's
// own files. So the four need values name exactly the things a workbench
// cannot provide, and declaring any of them is by construction a mismatch.
// Encoding that as a table rather than as `len(needs) > 0` is deliberate --
// the day a GPU-bearing workbench flavour exists, one `false` becomes `true`
// and nothing else in this file moves.
//
// Refusing on the hint replaces a failure that arrived three layers down and
// named nothing: a `defaults read` on Linux, an xdotool with no DISPLAY, a
// path under /Users that is simply not there. Those read to the model as "the
// command is wrong" and it retries with variations.
//
// WHAT THIS FILE DELIBERATELY DOES NOT DO is reroute. See the comment at
// evaluateEnvironmentHint.
//
// The names exported here are the seam the agent tool loop consumes to decide
// what to do with a mismatch; they are part of this package's public surface
// and their JSON spellings are part of the tool result contract.

// ErrCodeEnvironmentMismatch is the dispatchResult.errorCode a workbench call
// carries when the caller's environment hint names something a workbench is
// not. The action did NOT run: no workspace was provisioned, no command was
// executed, nothing was fetched. The tool loop keys on this string.
const ErrCodeEnvironmentMismatch = "environment_mismatch"

// ErrCodeInvalidEnvironmentHint is the dispatchResult.errorCode for a
// malformed hint -- a non-object `environment`, a non-list `needs`, a
// non-string element, or a need value outside the closed set.
//
// It is a SEPARATE code from the mismatch on purpose. A mismatch is a fact
// about the workbench and the tool loop may act on it; an invalid hint is the
// caller getting the contract wrong, and the only useful response is to fix
// the call. Folding the two together would let a typo ("macos-tooling") read
// as "the workbench cannot do this", which is a reroute to the user's machine
// on the strength of a hyphen.
const ErrCodeInvalidEnvironmentHint = "invalid_environment_hint"

// The closed `needs` vocabulary. These are INPUT values: what the caller may
// declare the action requires.
const (
	// NeedDisplay -- the action drives a GUI / needs an X or Wayland display.
	NeedDisplay = "display"
	// NeedGPU -- the action needs GPU compute or hardware acceleration.
	NeedGPU = "gpu"
	// NeedMacOSTooling -- the action needs macOS-only tooling (Xcode,
	// `defaults`, `osascript`, the system frameworks).
	NeedMacOSTooling = "macos_tooling"
	// NeedUserFiles -- the action needs files that live on the user's own
	// machine. The workbench starts empty and never sees them.
	NeedUserFiles = "user_files"
)

// UnmetNeedOS is an OUTPUT-only reason: it appears in
// EnvironmentMismatch.UnmetNeeds when the hint's `os` names a platform this
// node is not, and it is NOT accepted as an input `needs` value (passing it
// is an invalid hint, like any other unknown value).
//
// It is in the same list rather than beside it so a consumer that reads only
// UnmetNeeds is never handed an empty list on a genuine mismatch. RequestedOS
// / WorkbenchOS carry the detail for anyone who wants it.
const UnmetNeedOS = "os"

// workbenchProvides answers, for each declared need, whether a workbench can
// satisfy it. Every answer is currently false, which is the whole point of the
// vocabulary -- these are the four things the workbench is not. Written as a
// table so the claim is inspectable and a future flavour can flip one entry.
var workbenchProvides = map[string]bool{
	NeedDisplay:      false,
	NeedGPU:          false,
	NeedMacOSTooling: false,
	NeedUserFiles:    false,
}

// EnvironmentNeeds returns the closed set of accepted `needs` values, sorted.
// Exported so a caller-side validator (the agent tool loop, a tool schema
// generator) can enumerate the vocabulary without restating it.
func EnvironmentNeeds() []string {
	out := make([]string, 0, len(workbenchProvides))
	for n := range workbenchProvides {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// EnvironmentMismatch is the machine-readable body of an environment_mismatch
// result. It rides the existing dispatchResult.Payload field, so the whole
// result is the same shape every other workbench failure has -- the tool loop
// reads errorCode as it always did and only reaches for this when the code is
// ErrCodeEnvironmentMismatch.
type EnvironmentMismatch struct {
	// UnmetNeeds names every reason the workbench cannot serve this call, drawn
	// from the four need constants plus UnmetNeedOS. Never empty in a mismatch.
	UnmetNeeds []string `json:"unmetNeeds"`
	// RequestedOS is the hint's `os` when it was supplied, empty otherwise.
	RequestedOS string `json:"requestedOs,omitempty"`
	// WorkbenchOS is the GOOS of the node that evaluated the hint.
	WorkbenchOS string `json:"workbenchOs"`
}

// EnvironmentMismatchFromPayload reads the structured mismatch off a workbench
// dispatch result payload (the JSON on the single MemoryNode a workbench call
// returns). Returns ok=false for any payload that is not an
// environment_mismatch, so a caller can pass every workbench result through it
// without pre-checking.
//
// This is the reader half of the seam: the tool loop should NOT re-derive the
// needs by parsing the error message.
func EnvironmentMismatchFromPayload(payload []byte) (EnvironmentMismatch, bool) {
	if len(payload) == 0 {
		return EnvironmentMismatch{}, false
	}
	var envelope struct {
		OK        bool                `json:"ok"`
		ErrorCode string              `json:"errorCode"`
		Payload   EnvironmentMismatch `json:"payload"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return EnvironmentMismatch{}, false
	}
	if envelope.OK || envelope.ErrorCode != ErrCodeEnvironmentMismatch {
		return EnvironmentMismatch{}, false
	}
	return envelope.Payload, true
}

// environmentHint is the parsed form of the builtin's optional `environment`
// argument.
type environmentHint struct {
	present bool
	os      string
	needs   []string
}

// parseEnvironmentHint reads the `environment` argument. A missing or nil
// value is "no hint" and yields (zero, nil) -- every pre-#4353 caller, and
// every action that genuinely does not care. There is no default hint;
// guessing one would fire the mismatch on calls that would have worked.
//
// Every other malformed shape is an error, including an unknown need value.
// Silently dropping one would mean the action runs having been told it needs
// something nobody checked, which is the exact failure the hint exists to
// remove.
func parseEnvironmentHint(raw any) (environmentHint, error) {
	if raw == nil {
		return environmentHint{}, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return environmentHint{}, fmt.Errorf("`environment` must be an object of {os, needs}, got %T", raw)
	}
	if len(obj) == 0 {
		return environmentHint{}, nil
	}
	hint := environmentHint{present: true}

	if rawOS, has := obj["os"]; has && rawOS != nil {
		s, ok := rawOS.(string)
		if !ok {
			return environmentHint{}, fmt.Errorf("`environment.os` must be a string, got %T", rawOS)
		}
		hint.os = strings.ToLower(strings.TrimSpace(s))
	}

	rawNeeds, has := obj["needs"]
	if !has || rawNeeds == nil {
		return hint, nil
	}
	items, err := stringList(rawNeeds)
	if err != nil {
		return environmentHint{}, fmt.Errorf("`environment.needs`: %w", err)
	}
	seen := map[string]bool{}
	for _, item := range items {
		need := strings.ToLower(strings.TrimSpace(item))
		if need == "" {
			continue
		}
		if _, known := workbenchProvides[need]; !known {
			return environmentHint{}, fmt.Errorf(
				"`environment.needs` contains %q, which is not one of %s",
				item, strings.Join(EnvironmentNeeds(), " / "))
		}
		if seen[need] {
			continue
		}
		seen[need] = true
		hint.needs = append(hint.needs, need)
	}
	return hint, nil
}

// stringList normalises the JSON shapes a list of strings arrives in.
func stringList(raw any) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("must be a list of strings, found a %T element", item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("must be a list of strings, got %T", raw)
	}
}

// evaluateEnvironmentHint compares the hint with what a workbench IS and
// returns the mismatch when there is one, or nil when the workbench can serve
// the call.
//
// THIS FUNCTION REFUSES; IT DOES NOT REDIRECT. The decision to run the work on
// the user's own machine instead belongs to the agent tool loop, which is the
// only layer that can see whether the plan already holds standing scope and
// whether the task was approved. The knowledge corpus states the rule the
// refusal exists to keep enforceable (integrations/knowledge/seed.go, the
// `workbench:failureFallback` chunk):
//
//	"When the workbench can't do the job (the task genuinely requires macOS /
//	Xcode / a GUI app on the user's machine / a file the user has locally),
//	DON'T silently switch to computer-use, and DON'T just dead-end with 'I
//	can't.' Close the loop through the user-gated escalation path"
//
// -- which for a plan without standing scope means `requestComputerUseScope`
// and a consent card the user clicks, not a switch this function could make on
// its own. A dispatcher that quietly re-pointed the call at the user's machine
// would be doing computer-use on the strength of a hint the model wrote.
//
// The GOOS compared against is this node's. On the forwarded path that is the
// agent node rather than the workbench node it will hand the call to; both are
// the same engine image in the same cluster, so the answer is the same, and
// the alternative (a round trip to ask) would spend a hop to learn `linux`.
func evaluateEnvironmentHint(hint environmentHint) *EnvironmentMismatch {
	if !hint.present {
		return nil
	}
	var unmet []string
	if hint.os != "" && hint.os != runtime.GOOS {
		unmet = append(unmet, UnmetNeedOS)
	}
	for _, need := range hint.needs {
		if !workbenchProvides[need] {
			unmet = append(unmet, need)
		}
	}
	if len(unmet) == 0 {
		return nil
	}
	sort.Strings(unmet)
	return &EnvironmentMismatch{
		UnmetNeeds:  unmet,
		RequestedOS: hint.os,
		WorkbenchOS: runtime.GOOS,
	}
}

// describeMismatch is the human half of the refusal -- the sentence the model
// reads when it does not read the structured payload. It names what was asked
// for and what a workbench is, because "environment mismatch" on its own is
// the same undiagnosable shape the hint replaced.
func describeMismatch(m EnvironmentMismatch) string {
	var b strings.Builder
	b.WriteString("workbench: this call declared environment needs the workbench cannot meet (")
	b.WriteString(strings.Join(m.UnmetNeeds, ", "))
	b.WriteString("). A workbench is a headless ")
	b.WriteString(m.WorkbenchOS)
	b.WriteString(" sandbox in the cluster: no display, no GPU, no macOS tooling, and none of the user's own files.")
	if m.RequestedOS != "" && m.RequestedOS != m.WorkbenchOS {
		fmt.Fprintf(&b, " The call asked for %s.", m.RequestedOS)
	}
	b.WriteString(" The action was NOT run. If it genuinely needs the user's own machine, escalate through the" +
		" consent path (requestComputerUseScope) rather than assuming a switch was made for you.")
	return b.String()
}
