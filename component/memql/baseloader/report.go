package baseloader

// report.go adds a structured skip sink to the generic load pipeline
// (epic #2351 / S2, memql#2357). S1 (#2356) raised every per-slice
// parse/register failure from Debug to WARN so a dropped construct is
// visible in logs; S2 additionally RECORDS each drop as a structured
// Skip so the caller can build a LoadReport, print a boot summary, and
// (for the embedded core tree + registered packs) refuse to boot unless
// an operator sets the break-glass MEMQL_DSL_ALLOW_SKIPS.
//
// The sink lives in baseloader (not the memql package) so LoadOne /
// LoadMany can append to it without a circular import -- memql imports
// baseloader, never the reverse. The memql-side LoadReport embeds these
// Skip atoms alongside the S5 duplicate detections and the durable-
// bundle re-hydration quarantines.

// Skip is one construct a loader could not register: it either failed to
// parse or failed to register into its registry. It carries enough to
// name the construct in a strict-boot failure and in the startup log --
// the loader scope, the construct keyword, the construct name, the
// origin file, the phase that failed, and the error text.
type Skip struct {
	Component string // loader scope, e.g. "memql.unifiedShapeLoader"
	Keyword   string // construct keyword: "shape", "spec", "tool", ...
	Name      string // construct name
	File      string // origin file path in the DSL tree
	Phase     string // "parse" | "register" (or a loader-specific phase)
	Err       string // error text
}

// Report is the skip sink LoadOne / LoadMany append to. A nil *Report is
// a valid no-op sink -- Add guards on nil -- so existing callers that do
// not want a report keep working unchanged (they pass no sink at all via
// the variadic parameter).
type Report struct {
	Skips []Skip
}

// Add appends a Skip to the report. Nil-safe: a nil receiver is a no-op,
// which is what lets LoadOne / LoadMany accept an optional sink.
func (r *Report) Add(s Skip) {
	if r == nil {
		return
	}
	r.Skips = append(r.Skips, s)
}

// firstSink returns the first non-nil sink from a variadic list, or nil
// when none was supplied. Used by LoadOne / LoadMany to accept an
// optional trailing *Report without breaking the many existing callers
// that pass none.
func firstSink(sinks []*Report) *Report {
	for _, s := range sinks {
		if s != nil {
			return s
		}
	}
	return nil
}
