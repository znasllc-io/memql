// Package maketargets answers one question in one place: does this
// backtick-quoted `make <target>` citation name a target the Makefile
// actually has?
//
// It exists because the answer was assumed rather than checked, and the
// assumption was wrong across dozens of sites (memql#4405). Error text an
// operator reads at the moment of failure directed them at `secret-set`,
// `secrets-seed` and `dev-refresh` make targets; the Makefile has never had
// any of them. The tooling underneath was real -- `scripts/secrets` takes
// `seed` and `health` -- so what was missing was never a feature, only the
// wrapper every caller had gone on citing.
//
// Note how the dead names are written above: WITHOUT `make` inside the
// backticks. That is the convention this package enforces and therefore has
// to obey. A backtick-quoted `make <target>` span is read as a DIRECTIVE --
// something a human is being told to run -- so a name that is not a target
// must be spelled some other way. Saying "this used to say `make foo`" would
// be prose about a nonexistent command that is indistinguishable, to any
// checker, from an instruction to run one.
//
// Two things made it invisible for so long, and both are the reason this is a
// package rather than a regexp at one call site:
//
//   - Two tests asserted `strings.Contains(err.Error(), "make secret-set")`
//     as a proxy for "the seeding hint survived". The assertion passed
//     BECAUSE the falsehood was there, so the only automated opinion on the
//     subject was voting for the bug.
//   - The citations are spread across Go string literals, Go comments and
//     Markdown, and no one of those surfaces looked wrong on its own.
//
// The extractor and the target parser live together so a checker and the
// thing it checks cannot drift into disagreeing about what a citation is.
package maketargets

import (
	"regexp"
	"sort"
	"strings"
)

// targetLine matches a Makefile target definition at the start of a line.
//
// Deliberately the same expression the issue measured with
// (`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*:`), so the set this package calls "real" is
// the set that measurement was taken over. Pattern rules (`%.o:`), variable
// assignments and `.PHONY`-style dot targets are excluded by the leading
// character class, which is what we want: none of them is something a human
// types after `make`.
var targetLine = regexp.MustCompile(`(?m)^([a-zA-Z0-9_][a-zA-Z0-9_.-]*):`)

// citation matches a backtick-quoted `make <target>` span.
//
// The target is the FIRST word after `make`, so `make up SERVERS=2` and
// `make secret-set NAME=X VALUE=Y` both resolve to their target alone. The
// character after the target is captured so a family reference
// (`make secrets-*`) can be told apart from a command.
var citation = regexp.MustCompile("`make[ \t]+([a-zA-Z0-9_][a-zA-Z0-9_.-]*)(.?)")

// Targets parses the target names a Makefile defines.
func Targets(makefileSrc string) map[string]bool {
	out := map[string]bool{}
	for _, m := range targetLine.FindAllStringSubmatch(makefileSrc, -1) {
		out[m[1]] = true
	}
	return out
}

// Citation is one backtick-quoted `make <target>` span found in a text.
type Citation struct {
	// Target is the first word after `make`.
	Target string
	// Line is 1-indexed, so it pastes into an editor.
	Line int
	// Family is true for a `make <prefix>-*` reference, which names a
	// FAMILY of targets rather than one command. A family is satisfied by
	// any real target carrying the prefix.
	Family bool
}

// Citations extracts every backtick-quoted `make <target>` span from text.
func Citations(text string) []Citation {
	var out []Citation
	for _, loc := range citation.FindAllStringSubmatchIndex(text, -1) {
		target := text[loc[2]:loc[3]]
		trailing := ""
		if loc[4] >= 0 {
			trailing = text[loc[4]:loc[5]]
		}
		out = append(out, Citation{
			Target: target,
			Line:   1 + strings.Count(text[:loc[0]], "\n"),
			Family: trailing == "*",
		})
	}
	return out
}

// Unknown returns the citations in text that name no real target.
//
// A family reference is unknown only when NO real target carries its prefix
// -- which is the failure docs/public/operate/env-vars.md carried for its
// whole life: a documented `secrets-*` workflow where not one `secrets-`
// target has ever existed.
func Unknown(text string, real map[string]bool) []Citation {
	var out []Citation
	for _, c := range Citations(text) {
		if c.Family {
			if !anyWithPrefix(real, c.Target) {
				out = append(out, c)
			}
			continue
		}
		if !real[c.Target] {
			out = append(out, c)
		}
	}
	return out
}

// UnknownTargets is Unknown reduced to the distinct target names, sorted --
// the form an error message wants when it is reporting a whole tree.
func UnknownTargets(text string, real map[string]bool) []string {
	seen := map[string]bool{}
	for _, c := range Unknown(text, real) {
		seen[c.Target] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func anyWithPrefix(real map[string]bool, prefix string) bool {
	for t := range real {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}
