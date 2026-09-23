package procedure

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
)

// preconditions.go is D16's first line of drift detection: a procedure's
// preconditions are the environment predicates that were true at EVERY
// successful start -- an initiation set in the sense of Konidaris and Barto's
// skill chaining, learned from the recordings rather than written by anyone
// -- and a replay compares them to the target before it acts.
//
// The starts are read from the session fingerprint the cockpit records as a
// recording's first event (memql-cockpit internal/worker/harness/record.go,
// type Fingerprint): {type, v, seq, takenAt, app, platform{os, arch},
// tools[{name, version}], cwd, cwdDigest?, cwdEntries?, cwdTruncated?,
// variables[{name, set, digest?}], inputs[...]}. The engine reads it back from
// a step row as decoded JSON, which is why LearnPreconditions takes maps.

// Preconditions is a learned initiation set. Every field is a predicate that
// held at every recorded start; an absent one was not learned, which is
// different from one learned false.
type Preconditions struct {
	Platform       map[string]string `json:"platform,omitempty"`       // os, arch
	Tools          map[string]string `json:"tools,omitempty"`          // name -> version (only tools the procedure's exec steps invoke)
	Variables      map[string]string `json:"variables,omitempty"`      // name -> VariableUnset | digest
	EmptyWorkspace *bool             `json:"emptyWorkspace,omitempty"` // every recorded start had cwdEntries == 0
}

// VariableUnset is a variable's value in Preconditions when it was not set.
// Unset and set-to-empty are different facts -- the cockpit carries `set` even
// when false for that reason -- and a set variable is always a digest, so the
// word cannot collide with one.
const VariableUnset = "unset"

// The replay targets, spelled as component/work spells them (ReplayTarget).
// This module may not import component/work, so the words are repeated here
// and a caller that holds both can pin them together in a test; only
// TargetWorkbench changes what CheckPreconditions compares.
const (
	TargetWorkbench = "workbench"
	TargetMachine   = "machine"
)

// LearnPreconditions keeps the predicates EVERY start agreed on, and among the
// tools only those the procedure invokes (usedTools, UsedTools' answer): the
// fingerprint probes every common toolchain, and a precondition on one the
// procedure never runs would refuse replays for a reason that cannot matter.
//
// A start with no fingerprint at all (nil or empty) is not evidence: it
// measured nothing, and letting it veto every predicate would make one lost
// chunk erase the whole set -- the permissive direction. A fingerprint that IS
// present but could not measure something (no cwdEntries because the listing
// failed) did not agree on it, and that predicate is dropped.
//
// EmptyWorkspace is only ever learned TRUE. A workspace empty at some starts
// and not at others says nothing, and "must not be empty" is a claim no
// recording made.
func LearnPreconditions(fingerprints []map[string]any, usedTools []string) Preconditions {
	var starts []startFacts
	for _, fp := range fingerprints {
		if len(fp) == 0 {
			continue
		}
		starts = append(starts, readStart(fp))
	}
	var out Preconditions
	if len(starts) == 0 {
		return out
	}

	for _, key := range []string{"arch", "os"} {
		if v, ok := agreed(starts, func(s startFacts) map[string]string { return s.platform }, key); ok {
			out.Platform = put(out.Platform, key, v)
		}
	}

	seen := map[string]bool{}
	for _, name := range usedTools {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if v, ok := agreed(starts, func(s startFacts) map[string]string { return s.tools }, name); ok {
			out.Tools = put(out.Tools, name, v)
		}
	}

	for _, name := range sortedKeysOf(starts[0].variables) {
		if v, ok := agreed(starts, func(s startFacts) map[string]string { return s.variables }, name); ok {
			out.Variables = put(out.Variables, name, v)
		}
	}

	empty := true
	for _, s := range starts {
		if s.cwdEntries == nil || *s.cwdEntries != 0 {
			empty = false
			break
		}
	}
	if empty {
		out.EmptyWorkspace = &empty
	}
	return out
}

// startFacts is one fingerprint, reduced to the predicates a precondition can
// be learned from. A fact the fingerprint did not carry is absent here too.
type startFacts struct {
	platform   map[string]string
	tools      map[string]string // name -> normalized version
	variables  map[string]string // name -> VariableUnset | digest
	cwdEntries *int
}

func readStart(fp map[string]any) startFacts {
	s := startFacts{platform: map[string]string{}, tools: map[string]string{}, variables: map[string]string{}}
	if p, ok := fp["platform"].(map[string]any); ok {
		for _, key := range []string{"os", "arch"} {
			if v, ok := p[key].(string); ok && v != "" {
				s.platform[key] = v
			}
		}
	}
	for _, e := range objectList(fp["tools"]) {
		name, _ := e["name"].(string)
		version, _ := e["version"].(string)
		if name != "" && strings.TrimSpace(version) != "" {
			s.tools[name] = normalizeToolVersion(version)
		}
	}
	for _, e := range objectList(fp["variables"]) {
		name, _ := e["name"].(string)
		if name == "" {
			continue
		}
		set, known := e["set"].(bool)
		digest, _ := e["digest"].(string)
		switch {
		case known && !set:
			s.variables[name] = VariableUnset
		case known && set && digest != "":
			s.variables[name] = digest
			// A variable reported set but with no digest was not measured:
			// its value is unknown, and unknown is not agreement.
		}
	}
	s.cwdEntries = wholeNumber(fp["cwdEntries"])
	return s
}

// objectList reads a JSON list of objects in either decoded shape: []any from
// encoding/json, or []map[string]any from a caller that built it in Go.
func objectList(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// wholeNumber reads a non-negative integer in any of the shapes a decoded
// number arrives in. Anything else -- a fraction, a negative, a string -- is
// not a count of entries, and is treated as unmeasured.
func wholeNumber(v any) *int {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case int:
		f = float64(t)
	case int64:
		f = float64(t)
	case json.Number:
		parsed, err := t.Float64()
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	if f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		return nil
	}
	n := int(f)
	return &n
}

// agreed reports the value every start has for key, when every start has the
// SAME one.
func agreed(starts []startFacts, of func(startFacts) map[string]string, key string) (string, bool) {
	first, ok := of(starts[0])[key]
	if !ok {
		return "", false
	}
	for _, s := range starts[1:] {
		if v, ok := of(s)[key]; !ok || v != first {
			return "", false
		}
	}
	return first, true
}

func put(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// versionToken matches the version number inside one word of a tool's version
// line: an optional "v" or "go" prefix, dotted digits, and whatever suffix
// rides on them (rc1, +build, -beta).
var versionToken = regexp.MustCompile(`^(?:v|go)?(\d+(?:\.\d+)*[0-9A-Za-z.+~-]*)$`)

// normalizeToolVersion reduces a tool's own version line to its version
// number: "git version 2.43.0" -> "2.43.0", "v22.1.0" -> "22.1.0",
// "go version go1.22.1 darwin/arm64" -> "1.22.1".
//
// It exists because the line is the tool's choice, and `go version` names the
// PLATFORM in it. Compared as lines, a Go procedure recorded on a Mac could
// never replay on the Linux workbench: the platform the workbench deliberately
// does not compare would come back in through the tool line. A line with no
// version-shaped word is kept whole, which still compares exactly. It is
// idempotent, so a caller may hand CheckPreconditions raw lines or learned
// numbers alike.
func normalizeToolVersion(raw string) string {
	line := strings.TrimSpace(raw)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	for _, word := range strings.Fields(line) {
		word = strings.TrimRight(word, ",;:)")
		if m := versionToken.FindStringSubmatch(word); m != nil {
			return m[1]
		}
	}
	return line
}

// PreconditionReport is the outcome of one check. Held is true only when
// nothing mismatched AND nothing learned went unmeasured.
type PreconditionReport struct {
	Held       bool     `json:"held"`
	Mismatches []string `json:"mismatches,omitempty"` // "tools.node: recorded 22.1.0, found 20.3.0"
	Unmeasured []string `json:"unmeasured,omitempty"` // a learned predicate the observation does not carry
}

// CheckPreconditions compares a learned initiation set with what the target
// reports, before the first step.
//
// On the WORKBENCH the platform and the variables are not compared. A
// procedure reaches the workbench only because its footprint is portable
// (D4), and a portable footprint is platform-independent by definition; the
// person's environment variables do not exist there. The tools the
// procedure's own commands use ARE compared, and so is the workspace. Any
// other target -- the person's machine, or a target nobody named -- compares
// everything: relaxing a check needs a reason, and only the workbench has one.
//
// An ABSENT measurement is never a match: a learned predicate the observation
// does not carry is Unmeasured and the check does not hold, because a replay
// that assumed the tool was there would find out halfway through.
func CheckPreconditions(learned, observed Preconditions, target string) PreconditionReport {
	var r PreconditionReport
	everything := target != TargetWorkbench

	compare := func(name, recorded string, found string, present bool, same func(a, b string) bool) {
		switch {
		case !present:
			r.Unmeasured = append(r.Unmeasured, name)
		case !same(recorded, found):
			r.Mismatches = append(r.Mismatches, name+": recorded "+recorded+", found "+found)
		}
	}
	exact := func(a, b string) bool { return a == b }
	byVersion := func(a, b string) bool { return normalizeToolVersion(a) == normalizeToolVersion(b) }

	// A value present but empty is as unmeasured as an absent one: a prober
	// that wrote "" for a tool could not run it.
	measured := func(m map[string]string, k string) (string, bool) {
		v, ok := m[k]
		return v, ok && strings.TrimSpace(v) != ""
	}
	if everything {
		for _, key := range sortedKeysOf(learned.Platform) {
			found, ok := measured(observed.Platform, key)
			compare("platform."+key, learned.Platform[key], found, ok, exact)
		}
	}
	for _, name := range sortedKeysOf(learned.Tools) {
		found, ok := measured(observed.Tools, name)
		compare("tools."+name, normalizeToolVersion(learned.Tools[name]), normalizeToolVersion(found), ok, byVersion)
	}
	if everything {
		for _, name := range sortedKeysOf(learned.Variables) {
			found, ok := measured(observed.Variables, name)
			compare("variables."+name, learned.Variables[name], found, ok, exact)
		}
	}
	if learned.EmptyWorkspace != nil {
		want := boolWord(*learned.EmptyWorkspace)
		if observed.EmptyWorkspace == nil {
			compare("emptyWorkspace", want, "", false, exact)
		} else {
			compare("emptyWorkspace", want, boolWord(*observed.EmptyWorkspace), true, exact)
		}
	}
	r.Held = len(r.Mismatches) == 0 && len(r.Unmeasured) == 0
	return r
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// shells are the command words whose -c argument is itself a command line.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// controlOperators separate the simple commands of a command line, so the
// word after one is a command word again.
var controlOperators = map[string]bool{"&&": true, "||": true, ";": true, "|": true, "&": true, "|&": true}

// UsedTools returns the tools a procedure's exec steps invoke: every COMMAND
// WORD of each step's command, by basename, sorted -- basename because the
// fingerprint names a tool the way PATH resolves it.
//
// Every command word, not only argv[0], because the first word is usually not
// the tool. `cd app && npm test` invokes npm, and Codex runs EVERY command as
// `bash -lc '<script>'`: reading argv[0] alone would give a Codex procedure no
// tool preconditions at all. So the command line is read the way a shell
// reads its simple commands -- a word after a control operator (&&, ||, ;, |,
// &) is a command word again, a NAME=value before one is an assignment, and
// the -c argument of a shell is a command line of its own. It is still not a
// shell: it does not expand anything, and a wrapper like `env` or `sudo` is
// reported as itself. Over-reporting is harmless -- LearnPreconditions keeps
// only tools the fingerprints probed -- and under-reporting is what would
// weaken the check.
//
// A command word that is a hole names no one tool and is skipped.
func UsedTools(t Template) []string {
	seen := map[string]bool{}
	for _, s := range t.Steps {
		if s.Tool != "exec" || s.Args == nil {
			continue
		}
		for _, key := range sortedCommandKeys() {
			n, ok := s.Args.At([]string{key})
			if !ok || n == nil {
				continue
			}
			for _, w := range commandWordsOf(n) {
				seen[w] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func sortedCommandKeys() []string {
	keys := make([]string, 0, len(commandKeys))
	for k := range commandKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// holeWord stands for an argument that is a hole: its text is unknown, and it
// can be neither a command word nor a script.
const holeWord = "\x00hole"

// commandWordsOf reads one command argument in whichever shape it was
// recorded: a command line (a string, parsed into argv), or an ARGUMENT VECTOR
// (a list, executed directly with no shell between its elements).
func commandWordsOf(n *Node) []string {
	switch {
	case n.Kind == KindLit:
		return commandLineWords(words(splitArgv(n.Lit)))
	case n.Kind == KindArray && n.Form == FormArgv:
		return commandLineWords(words(n.Kids))
	case n.Kind == KindArray:
		return argumentVectorWords(words(n.Kids))
	}
	return nil
}

// words is each element's text, or holeWord for an element that has none.
func words(kids []*Node) []string {
	out := make([]string, len(kids))
	for i, k := range kids {
		switch {
		case k == nil || k.Kind == KindHole:
			out[i] = holeWord
		case k.Kind == KindLit:
			out[i] = k.Lit
		default:
			v, err := Materialize(k, nil)
			s, ok := v.(string)
			if err != nil || !ok {
				out[i] = holeWord
				continue
			}
			out[i] = s
		}
	}
	return out
}

// argumentVectorWords reads a vector executed with no shell: its first
// element is the one command word, unless it is a shell whose -c argument is a
// command line.
func argumentVectorWords(argv []string) []string {
	if len(argv) == 0 || argv[0] == holeWord {
		return nil
	}
	word := basename(argv[0])
	out := []string{word}
	if shells[word] {
		out = append(out, shellScriptWords(argv[1:])...)
	}
	return out
}

// commandLineWords reads a command line's simple commands.
func commandLineWords(tokens []string) []string {
	var out []string
	expectCommand := true
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if controlOperators[tok] {
			expectCommand = true
			continue
		}
		ends := strings.HasSuffix(tok, ";") && tok != ";"
		if expectCommand && tok != holeWord && !isAssignment(tok) {
			word := basename(strings.TrimSuffix(tok, ";"))
			if word != "" {
				out = append(out, word)
				if shells[word] {
					end := i + 1
					for end < len(tokens) && !controlOperators[tokens[end]] {
						end++
					}
					out = append(out, shellScriptWords(tokens[i+1:end])...)
				}
			}
			expectCommand = false
		} else if expectCommand && tok == holeWord {
			expectCommand = false
		}
		if ends {
			expectCommand = true
		}
	}
	return out
}

// shellScriptWords finds a shell's -c flag among its arguments and reads the
// argument after it as a command line.
func shellScriptWords(args []string) []string {
	for i, a := range args {
		if isShortFlagWithC(a) && i+1 < len(args) && args[i+1] != holeWord {
			return commandLineWords(words(splitArgv(args[i+1])))
		}
	}
	return nil
}

// isShortFlagWithC matches -c, -lc, -ec: a cluster of one-letter options one
// of which is c. A long option (--rcfile) is not one, whatever it contains.
func isShortFlagWithC(a string) bool {
	return len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], 'c')
}

// isAssignment is NAME=value, which a shell reads as an assignment before a
// command rather than as the command.
func isAssignment(tok string) bool {
	name, _, found := strings.Cut(tok, "=")
	if !found || name == "" {
		return false
	}
	for i, r := range name {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func basename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
