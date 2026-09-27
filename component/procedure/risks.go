package procedure

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// risks.go is what a replay may put in a parameter, judged before it starts
// (CheckBindings). Strict quoting makes any value ONE argument; this is about
// what that argument MEANS, against what every recording put there.

// CheckBindings judges the values a replay is about to bind -- hole id ->
// value, from the app's own actions in a shadow comparison or from the goal's
// input when the procedure serves -- against each hole's recorded shape
// (Hole.Shape). A value is refused when it shows a feature no recording
// showed: it begins with "-" and no recorded value was an option, with "/" and
// none was an absolute path, with "~" and none was under a home directory, or
// it climbs to a parent directory and none did. And ANY value is refused for a
// hole whose recorded value reached its command through a shell expansion:
// the value the program received was never recorded, so nothing a goal gives
// can stand for it.
//
// A hole with no shape -- a payload written before shapes -- is judged as if
// no recording showed any feature: the direction that refuses. A value for an
// id the template does not declare is not judged here; Materialize never
// writes it anywhere.
//
// The error is ONE sentence, naming the parameter and the value, because it is
// what the person whose goal was refused reads.
func CheckBindings(t Template, values map[string]string) error {
	holes := append([]Hole(nil), t.Holes...)
	sort.SliceStable(holes, func(i, j int) bool { return holes[i].Id < holes[j].Id })
	for _, h := range holes {
		v, bound := values[h.Id]
		if !bound {
			continue
		}
		var s HoleShape
		if h.Shape != nil {
			s = *h.Shape
		}
		why := ""
		switch {
		case s.Expands:
			why = "was recorded through a shell expansion, so no value can stand for what its command received"
		case strings.HasPrefix(v, "-") && !s.Dash:
			why = "was never recorded as an option"
		case strings.HasPrefix(v, "/") && !s.Rooted:
			why = "was never recorded as an absolute path"
		case strings.HasPrefix(v, "~") && !s.Home:
			why = "was never recorded under a home directory"
		case hasParentSegment(v) && !s.DotDot:
			why = "was never recorded climbing to a parent directory"
		}
		if why != "" {
			return &BindingRefusal{Hole: h.Id, Step: h.StepIndex, Value: v, Why: why}
		}
	}
	return nil
}

// BindingRefusal is CheckBindings' refusal: the parameter, the step it belongs
// to -- which a shadow comparison records as where the recording stopped being
// an instance -- the value, and why. Its Error is the one sentence.
type BindingRefusal struct {
	Hole  string
	Step  int
	Value string
	Why   string
}

func (e *BindingRefusal) Error() string {
	return fmt.Sprintf("parameter %s %s, and this goal gave it %s", e.Hole, e.Why, shownValue(e.Value))
}

// shownValueLimit bounds how much of a refused value a sentence quotes: the
// sentence is read by a person, and a goal's input can be any size.
const shownValueLimit = 80

// shownValue is a value as a refusal quotes it, cut at a rune boundary past
// shownValueLimit bytes.
func shownValue(v string) string {
	if len(v) <= shownValueLimit {
		return strconv.Quote(v)
	}
	cut := shownValueLimit
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return strconv.Quote(v[:cut]) + "..."
}

// ReplayRisks names what makes a template unfit to be promoted, however well
// its shadow comparisons go: one sentence per risk, in step order, empty when
// there is none. The person approving a promotion reads the steps; these are
// the things a reading of the steps does not show.
//
//   - a recorded instance the template does not bind (BindInstance false): the
//     template is not a generalization of that recording, so a replay compared
//     against the others is compared against a procedure the recordings never
//     showed. Arrays of different lengths leave exactly such a template -- a
//     gap hole only some recordings fill.
//   - a parameter whose shape Expands: the value its command received was
//     never recorded, and CheckBindings refuses every value for it.
//   - a parameter the command RUNS: a shell's -c script (the argument after a
//     short-flag cluster holding c, for sh, bash, zsh, dash, ksh or fish),
//     inline code of an interpreter (python and python3 -c; node -e, --eval,
//     -p, --print; perl and ruby -e, -E), or an argument of eval. Strict
//     quoting makes a value one argument, and these are arguments a program
//     executes. The program may be named anywhere in its simple command, so
//     `sudo sh -c <script>` and `env X=1 bash -c <script>` are found too.
//   - a parameter where a command's PROGRAM is named -- the first word of a
//     simple command, after its assignments -- or a whole command that is a
//     parameter: a goal's input must never choose what runs, the rule
//     replayable.go already holds a command's first word to.
//   - a parameter inside a here-document's body, or after a here-document or
//     here-string operator in its command: a document is text the shell hands
//     the command, expanding $ and backticks in it when its delimiter is
//     unquoted, and a value's quotes are more text there.
//
// Each applies to a command line (a FormArgv argv, read as the line
// Materialize sends) and to a command vector (an array under command, cmd or
// argv, as canonicalization builds one). The program list is short and NOT
// exhaustive -- ssh, xargs, find -exec, a make target all run code -- and it
// does not need to be: strict quoting makes every other position a single
// literal argument, and the steps are what a person approves.
func ReplayRisks(t Template, instances [][]Action) []string {
	failedAt := map[int][]int{} // step -> the instances that stopped fitting there
	for i, in := range instances {
		if _, step, ok := bindInstanceAt(t, in); !ok {
			failedAt[step] = append(failedAt[step], i)
		}
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for si := 0; si <= len(t.Steps); si++ {
		for _, i := range failedAt[si] {
			if si == len(t.Steps) {
				add(fmt.Sprintf("recorded instance %d has more steps than the template learned from it, so the template does not reproduce every recording", i))
				continue
			}
			add(fmt.Sprintf("recorded instance %d does not bind to step %d of the template learned from it, so the template does not reproduce every recording", i, si))
		}
		if si == len(t.Steps) {
			break
		}
		for _, h := range t.Holes {
			if h.StepIndex == si && h.Shape != nil && h.Shape.Expands {
				add(fmt.Sprintf("step %d's parameter %s was recorded through a shell expansion, so the value its command received was never recorded", si, h.Id))
			}
		}
		walkCommands(t.Steps[si].Args, "", func(n *Node) {
			for _, r := range commandRisks(n) {
				add(fmt.Sprintf("step %d's %s", si, r))
			}
		})
	}
	return out
}

// walkCommands calls fn for every command in an argument tree: a FormArgv
// argv, and any value under a command key -- a vector, a literal line, or a
// hole standing for the whole command. A command's own elements are not
// walked as commands.
func walkCommands(n *Node, key string, fn func(*Node)) {
	if n == nil {
		return
	}
	if commandKeys[key] || (n.Kind == KindArray && n.Form == FormArgv) {
		fn(n)
		return
	}
	switch n.Kind {
	case KindObject:
		for i, k := range n.Keys {
			walkCommands(n.Kids[i], k, fn)
		}
	case KindArray:
		for _, k := range n.Kids {
			walkCommands(k, key, fn)
		}
	}
}

// interpreterCodeFlags are the flags whose next argument an interpreter runs
// as code.
var interpreterCodeFlags = map[string]map[string]bool{
	"python":  {"-c": true},
	"python3": {"-c": true},
	"node":    {"-e": true, "--eval": true, "-p": true, "--print": true},
	"perl":    {"-e": true, "-E": true},
	"ruby":    {"-e": true, "-E": true},
}

// commandRisks is every risk in one command, each the rest of a sentence that
// begins with the step.
func commandRisks(n *Node) []string {
	switch {
	case n.Kind == KindHole:
		return []string{fmt.Sprintf("whole command is parameter %s, so a goal's input would choose what runs", n.HoleId)}
	case n.Kind == KindArray && n.Form == FormArgv:
		src, elems := argvSource(n)
		reading := readShell(src)
		out := wordRisks(reading.items, elems, true)
		for _, k := range reading.bodySlots {
			if id := holeIdOf(elems, k); id != "" {
				out = append(out, fmt.Sprintf("parameter %s is inside a here-document, whose text a shell reads without the quotes that make a value one argument", id))
			}
		}
		return out
	case n.Kind == KindArray:
		var (
			items []shellItem
			elems []*Node
		)
		for _, k := range n.Kids {
			if k != nil && k.Kind == KindLit {
				items = append(items, shellItem{word: k.Lit, here: strings.HasPrefix(k.Lit, "<<")})
				continue
			}
			items = append(items, shellItem{slots: []int{len(elems)}})
			elems = append(elems, k)
		}
		return wordRisks(items, elems, false)
	}
	return nil
}

// wordRisks reads the simple commands of a command for parameters that are
// code, a program, or a document's text. line is false for a vector, which no
// shell reads: its first element is the program whatever it looks like.
func wordRisks(items []shellItem, elems []*Node, line bool) []string {
	var out []string
	param := func(it shellItem) string {
		for _, k := range it.slots {
			if id := holeIdOf(elems, k); id != "" {
				return id
			}
		}
		return ""
	}
	for _, cmd := range simpleCommands(items) {
		c := 0
		for line && c < len(cmd) && len(cmd[c].slots) == 0 && isAssignment(cmd[c].word) {
			c++
		}
		if c < len(cmd) {
			if id := param(cmd[c]); id != "" {
				out = append(out, fmt.Sprintf("parameter %s is where its command's program is named, so a goal's input would choose the program that runs", id))
			}
		}
		afterHere := false
		for j := c; j < len(cmd); j++ {
			it := cmd[j]
			if id := param(it); id != "" && afterHere {
				out = append(out, fmt.Sprintf("parameter %s follows a here-document operator in its command, so a shell may read it as a document's text rather than as one argument", id))
			}
			afterHere = afterHere || it.here
			if len(it.slots) > 0 {
				continue
			}
			prog := basename(strings.TrimLeft(it.word, "("))
			switch {
			case shells[prog]:
				for k := j + 1; k < len(cmd); k++ {
					if len(cmd[k].slots) == 0 && isShortFlagWithC(cmd[k].word) {
						if k+1 < len(cmd) {
							if id := param(cmd[k+1]); id != "" {
								out = append(out, fmt.Sprintf("parameter %s is the script %s runs, so a goal's input would choose the code that runs", id, prog))
							}
						}
						break
					}
				}
			case interpreterCodeFlags[prog] != nil:
				for k := j + 1; k+1 < len(cmd); k++ {
					if len(cmd[k].slots) == 0 && interpreterCodeFlags[prog][cmd[k].word] {
						if id := param(cmd[k+1]); id != "" {
							out = append(out, fmt.Sprintf("parameter %s is code %s runs, so a goal's input would choose the code that runs", id, prog))
						}
					}
				}
			case prog == "eval":
				for k := j + 1; k < len(cmd); k++ {
					if id := param(cmd[k]); id != "" {
						out = append(out, fmt.Sprintf("parameter %s is an argument of eval, so a goal's input would choose the code that runs", id))
					}
				}
			}
		}
	}
	return out
}

// holeIdOf is the hole id of placeholder k, or "" when it stands for
// something that is not a hole.
func holeIdOf(elems []*Node, k int) string {
	if k < 0 || k >= len(elems) || elems[k] == nil || elems[k].Kind != KindHole {
		return ""
	}
	return elems[k].HoleId
}
