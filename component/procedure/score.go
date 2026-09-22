package procedure

import "sort"

// Hole prices. D13 says a free parameter is priced ABOVE a data-flow hole,
// "because a free parameter is something the caller has to supply". A constant
// is kept literal and costs the caller nothing at all.
//
// The ratio is what matters rather than the absolute numbers: a free parameter
// must be dear enough that a template made entirely of them cannot clear the
// floor, which is the record's own failure mode and the test beside it.
const (
	costFreeParam    = 3
	costDataFlowHole = 1
	costConstant     = 0
	// costCall is what one call site costs after the rewrite: the construct
	// name plus its argument list, charged once per use.
	costCall = 2
)

// Refusal reasons. A refusal that names itself is half of what makes the score
// falsifiable: an abstraction that was CONSIDERED and rejected, and why, is
// exactly what a reader checking the library needs.
const (
	ReasonOneUse      = "one use: D14's floor is two"
	ReasonNoGain      = "net compression is not positive"
	ReasonTooManyArgs = "free parameters exceed the argument ceiling"
	ReasonEmpty       = "the template has no steps"
)

// Utility is D14's score for one candidate abstraction.
type Utility struct {
	// Uses is the number of occurrences the abstraction would replace.
	Uses int
	// BodyCost is the size of the template's own body.
	BodyCost int
	// ArgCost is what its holes cost the caller, priced by class.
	ArgCost int
	// Saved is the corpus size the rewrite removes.
	Saved int
	// Net is Saved - BodyCost - ArgCost.
	Net int
	// Accepted is Net > 0 and Uses >= 2 and the argument ceiling held.
	Accepted bool
	// Reason names the refusal when Accepted is false.
	Reason string
}

// Candidate is a template plus where it occurs.
type Candidate struct {
	Template    Template
	Occurrences []Occurrence
}

// Accepted is one abstraction the library took, and the round it took it in.
type Accepted struct {
	Candidate Candidate
	Utility   Utility
	// Order is the acceptance round, from 0. The order IS the hierarchy: an
	// abstraction accepted at round n was scored against a corpus that already
	// contained rounds 0..n-1, so it may reference them.
	Order int
}

// Score is D14's compression utility: what the abstraction saves across its
// uses, against what its body and its arguments cost.
func Score(t Template, occurrences []Occurrence, corpus [][]Action, p Params) Utility {
	u := Utility{Uses: len(occurrences)}
	if len(t.Steps) == 0 {
		u.Reason = ReasonEmpty
		return u
	}

	for _, s := range t.Steps {
		u.BodyCost += s.Args.Size() + 1 // +1 for the tool itself
	}

	freeParams := 0
	for _, h := range t.Holes {
		switch h.Class {
		case HoleFree, HoleUnexplained:
			// An unexplained hole is priced as a free parameter: until a
			// derivation is checked and kept, the caller is the one who has to
			// supply it, and pricing it optimistically would accept a template
			// on evidence that does not exist yet.
			u.ArgCost += costFreeParam
			freeParams++
		case HoleDataFlow:
			u.ArgCost += costDataFlowHole
		case HoleConstant:
			u.ArgCost += costConstant
		default:
			u.ArgCost += costFreeParam
			freeParams++
		}
	}

	// What the corpus loses: each occurrence's own body, replaced by one call.
	for _, occ := range occurrences {
		u.Saved += occurrenceSize(occ, corpus) - costCall
	}
	u.Net = u.Saved - u.BodyCost - u.ArgCost

	switch {
	case u.Uses < 2:
		u.Reason = ReasonOneUse
	case freeParams > p.MaxArgs:
		u.Reason = ReasonTooManyArgs
	case u.Net <= 0:
		u.Reason = ReasonNoGain
	default:
		u.Accepted = true
	}
	return u
}

// occurrenceSize is the tree size the corpus spends on one occurrence. A
// position the corpus no longer holds -- because an earlier round rewrote it
// away -- contributes nothing, which is what makes the second round score
// against what is LEFT.
func occurrenceSize(occ Occurrence, corpus [][]Action) int {
	if occ.Sequence >= len(corpus) {
		return 0
	}
	seq := corpus[occ.Sequence]
	total := 0
	for _, pos := range occ.Positions {
		if pos < len(seq) && seq[pos].Args != nil {
			total += seq[pos].Args.Size() + 1
		}
	}
	return total
}

// Select grows the library ONE abstraction at a time: it accepts the
// max-utility candidate, rewrites the corpus, and repeats.
//
// The rewrite between rounds is the whole mechanism of D14's hierarchy. The
// second abstraction is scored against a corpus that already contains the
// first, so what it can still save is what is genuinely left -- and an
// abstraction accepted later may reference one accepted earlier. Scoring every
// candidate once against the original corpus would accept two overlapping
// abstractions that each claim the same saving.
func Select(candidates []Candidate, corpus [][]Action, p Params) []Accepted {
	work := make([][]Action, len(corpus))
	for i, seq := range corpus {
		work[i] = make([]Action, len(seq))
		for j, a := range seq {
			work[i][j] = a
			work[i][j].Args = a.Args.Clone()
		}
	}

	remaining := append([]Candidate(nil), candidates...)
	var out []Accepted
	for round := 0; len(remaining) > 0; round++ {
		type scored struct {
			idx int
			u   Utility
		}
		var best *scored
		for i := range remaining {
			u := Score(remaining[i].Template, remaining[i].Occurrences, work, p)
			if !u.Accepted {
				continue
			}
			if best == nil || betterUtility(u, remaining[i], best.u, remaining[best.idx]) {
				best = &scored{idx: i, u: u}
			}
		}
		if best == nil {
			return out
		}
		win := remaining[best.idx]
		out = append(out, Accepted{Candidate: win, Utility: best.u, Order: round})
		rewriteCorpus(work, win)
		remaining = append(remaining[:best.idx], remaining[best.idx+1:]...)
	}
	return out
}

// betterUtility is a TOTAL order, so two replicas selecting from one candidate
// set agree. Net first, then uses, then the smaller body, then the tool names
// -- the last tiebreak exists only so the answer never depends on slice order
// after a removal.
func betterUtility(a Utility, ca Candidate, b Utility, cb Candidate) bool {
	switch {
	case a.Net != b.Net:
		return a.Net > b.Net
	case a.Uses != b.Uses:
		return a.Uses > b.Uses
	case a.BodyCost != b.BodyCost:
		return a.BodyCost < b.BodyCost
	default:
		return toolsOf(ca.Template) < toolsOf(cb.Template)
	}
}

func toolsOf(t Template) string {
	names := make([]string, 0, len(t.Steps))
	for _, s := range t.Steps {
		names = append(names, s.Tool)
	}
	return joinNul(names)
}

func joinNul(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += "\x00"
		}
		out += v
	}
	return out
}

// rewriteCorpus replaces the winner's occurrences with a single call-sized
// action, so the next round scores against what is LEFT.
func rewriteCorpus(corpus [][]Action, win Candidate) {
	for _, occ := range win.Occurrences {
		if occ.Sequence >= len(corpus) {
			continue
		}
		positions := append([]int(nil), occ.Positions...)
		sort.Ints(positions)
		for i, pos := range positions {
			if pos >= len(corpus[occ.Sequence]) {
				continue
			}
			if i == 0 {
				// The first position becomes the call.
				corpus[occ.Sequence][pos].Args = Obj(nil)
				continue
			}
			corpus[occ.Sequence][pos].Args = nil
		}
	}
}
