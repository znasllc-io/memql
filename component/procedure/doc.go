// Package procedure turns recorded work-spine steps into a parameterized
// template, spending no model.
//
// The pipeline, in order, each stage a function over values:
//
//	Canonicalize(steps)            -> []Action    each step as tool + argument tree + digests
//	Symbolize(actions, params)     -> []Symbol    same-tool actions clustered by anti-unification distance
//	Mine(sequences, params)        -> []Pattern   closed frequent sub-sequences, gap-tolerant
//	Structure(sequences, params)   -> ProcessTree the inductive miner, so a retry is a loop node
//	Generalize(instances)          -> Template    instances anti-unified into holes
//	Classify(template, instances)  -> []Hole      data flow, then constant, then free (D13)
//	Score(template, corpus)        -> Utility     compression, two uses the floor (D14)
//	Select(candidates, corpus)     -> []Accepted  one at a time, rewriting between
//
// Nothing here reads a row or calls a provider; see purity_test.go. The one
// permitted model call in this epic proposes a derivation for a hole the rules
// could not explain, and integrations/procedure makes it -- what lives here is
// CheckDerivation, which decides whether the proposal holds on every recorded
// instance, and which is pure precisely so that the rejection is testable.
package procedure
