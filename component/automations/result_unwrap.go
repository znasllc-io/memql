package automations

// flatOutputer is implemented by the engine's *memql.ExecuteResult. We match it
// structurally (not by importing the memql package) to avoid an import cycle.
type flatOutputer interface {
	// FlatOutput returns (flatValue, true) when the result carries a flat
	// output payload (an object-literal map or a scalar return), and
	// (nil, false) for a Bundle-backed result.
	FlatOutput() (any, bool)
}

// UnwrapStepResult peels the Bundle-wrapped engine envelope off a stored step
// result so a downstream step reads the FLAT value the construct returned.
//
// A `logic` / `query` step stores its raw engine result (a
// *memql.ExecuteResult) as the step Result. An object-literal `return { ... }`
// or a scalar return lands its value in the envelope's flat output payload;
// reading it off the envelope -- `decide.result.x` / `field(decide.result,
// "x")` / the scalar `decide.result` -- resolved to nil at real-DB runtime,
// which blocked the decide->persist pattern (the two cognition logics in #2235;
// the engine-level root cause in #2271).
//
// ONLY a flat-output result is unwrapped. A Bundle-backed result (a query /
// node-list step) is returned UNCHANGED so the existing envelope-aware access
// path keeps working (e.g. `decide.result.Bundle.nodes`,
// `getGA.result.Bundle.nodes.0.id`). Non-envelope values (a scalar bool from
// `return true`, a plain map) are also returned unchanged, so the existing
// scalar-comparison gate (`steps.decide.result == true`) is unaffected.
func UnwrapStepResult(v any) any {
	if fo, ok := v.(flatOutputer); ok {
		if out, has := fo.FlatOutput(); has {
			return out
		}
	}
	return v
}
