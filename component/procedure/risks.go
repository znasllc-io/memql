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
			return fmt.Errorf("parameter %s %s, and this goal gave it %s", h.Id, why, shownValue(v))
		}
	}
	return nil
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
