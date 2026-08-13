package dslgate

// intrinsics.go is the `row.` namespace rule (memql#2779 for filters, #2786 for
// sort keys), moved to load time by memql#3629.
//
// A filter mixes two field surfaces under one syntax. Payload properties are
// BARE (epic #2292 -- the concept is bound by the construct signature), so
// without the rule a reader cannot tell `id == args.x` (row envelope) from
// `status == args.x` (payload) without memorising the reserved-word list, and
// the two compile to completely different SQL -- a table column versus a JSONB
// path. The same ambiguity decides an ORDER BY: `sort "id"` is either the row
// id or a payload property named `id`.
//
// Detection is shared with the edit-time Cockpit rule via sense's scanners,
// deliberately: the first cut of the filter gate had its own detector built on
// a `&&`-only splitter, so a bare intrinsic joined by `||` or wrapped in parens
// passed CI green while the editor flagged it. dsl/telephony/queries.memql
// already carried a parenthesized-OR filter, so that hole was reachable rather
// than theoretical. One detector, one answer -- and now one that also runs on a
// tree the tests never see.
//
// Scope is filter predicates and authored sort keys. A spec/trait body reads
// its signature-bound fields BARE and rejects `row.*` outright (epic #2281);
// mutation insert/update blocks write `id:` / `createdAt:` as target keys
// rather than references; the runtime and SDK sort surfaces still accept bare
// keys from callers. None of those are touched.

import (
	"fmt"

	"github.com/znasllc-io/memql/component/memql/sense"
)

func scanRowIntrinsics(path, src string) []Violation {
	var out []Violation
	for _, hit := range sense.ScanBareRowIntrinsics(src) {
		out = append(out, Violation{
			Gate: GateFilterRowIntrinsic,
			File: path,
			Line: hit.Line,
			Detail: fmt.Sprintf("filter names the row intrinsic %q bare -- write `row.%s`; the bare spelling is a PAYLOAD property and compiles to a different query (memql#2779)",
				hit.Text, hit.Name),
		})
	}
	for _, hit := range sense.ScanBareRowIntrinsicSortKeys(src) {
		out = append(out, Violation{
			Gate: GateSortRowIntrinsic,
			File: path,
			Line: hit.Line,
			Detail: fmt.Sprintf("sort key names the row intrinsic %q bare -- write \"row.%s\"; the bare spelling orders by a payload property of that name (memql#2786)",
				hit.Text, hit.Name),
		})
	}
	return out
}
