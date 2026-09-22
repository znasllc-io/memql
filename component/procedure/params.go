package procedure

// Params are the knobs of the pipeline. They are a VALUE and not a set of
// constants because the design record's cross-cutting rule says so: the
// symbolization budget, the mining support and gap and the argument ceiling
// are rows or manifest values with the defaults named here, so an operator can
// move them without a release.
type Params struct {
	// SymbolBudget is the greatest generalization distance at which an action
	// still joins a cluster. Too loose and every action of a tool collapses
	// into one symbol, which symbolize_test.go's negative control catches.
	SymbolBudget int
	// MinSupport is how many sequences a pattern must occur in. Two is the
	// floor of D14 and the lowest value that can mean anything.
	MinSupport int
	// Gap is how many unmatched symbols may fall between two adjacent
	// elements of an occurrence.
	Gap int
	// MaxArgs is the ceiling on a lifted procedure's free parameters. A
	// template needing more is not an abstraction, it is the corpus.
	MaxArgs int
	// Noise is the fraction of behaviour the inductive miner may discard
	// before it falls back to a flower model.
	Noise float64
}

// DefaultParams are the design record's values.
func DefaultParams() Params {
	return Params{SymbolBudget: 2, MinSupport: 2, Gap: 2, MaxArgs: 8, Noise: 0.2}
}
