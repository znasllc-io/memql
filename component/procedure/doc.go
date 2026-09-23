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
//
// The REPLAY half (epic memql#5408, decision D16) runs the other way, from a
// learned template back to the calls a replay makes, and is pure for the same
// reason -- every decision a replay takes can be checked on a literal:
//
//	Bind / BindInstance(template, action)  -> hole values   what a recording put in each hole
//	Materialize(node, values)              -> Go value      what a replay sends, back through Node.Form
//	LearnInputMap(template, instances, in) -> hole -> key   which goal input supplies each free parameter
//	LearnPreconditions(fingerprints, used) -> Preconditions the predicates every recorded start agreed on
//	CheckPreconditions(learned, observed)  -> report        compared before the first step
//	TokenReplay / PrefixFits(model, trace) -> fitness       the cheap running check after each step
//	Align(model, trace)                    -> Alignment     names the deviation once the check drops
//	AssignSymbol, SequenceTree                              a recording in a procedure's alphabet; its own model
//	MarshalTemplate, MarshalTree                            the stored form, checked in both directions
package procedure
