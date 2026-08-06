package dslfs

import "strings"

// domain.go owns the answers to "which directory segment of a .memql path
// means what", so boot and the lint lanes cannot drift about them
// (memql#2852). There are TWO questions, they have different answers, and
// both live here precisely so the difference is impossible to miss:
//
//	LAST segment   DomainFromFilePath      a file's namespace HINT, matched
//	                                       against a concept's canonical id
//	FIRST segment  RootDomainFromFilePath  the DIRECTORY a concept's canonical
//	                                       id is assembled under, and the
//	                                       domain a shape name is scoped to
//
// The FIRST-segment answer moved in beside its sibling during memql#3026's
// landing review. Before that this header said the file owned only the LAST
// one and pointed at three other implementations of the FIRST
// (unified_loader.go firstPathSegment, dslimports/symbols.go, integrity.go) --
// which is the drift the file exists to prevent, so the header was the defect
// rather than the placement. Those three remain; migrating them is follow-up
// work, and until it happens note that they differ on inputs the loader never
// emits (a legacy `v<digits>` first segment, a leading "./" or "/", a bare
// filename): RootDomainFromFilePath skips those segments, firstPathSegment
// returns them verbatim.
//
// Conflating the two is exactly how #2852's first fix opened a hole in both
// directions, and how #3026's first cut refused every ambient reference in a
// nested file. cmd/memqlmigrate/main.go records the same distinction from the
// other side (review #2614): the codemod deliberately uses the FIRST segment,
// "NOT the immediate parent", because nested files belong to the top-level
// domain for id-assembly purposes.
//
// The drift this exists to stop was measured. component/memql's boot resolver
// walked from the LAST directory segment backwards; dslimports'
// sameDomainConceptDecl took the FIRST path segment:
//
//	agents/tools/askSpecialist.memql   boot -> "tools"   lint -> "agents"
//
// The tree carries 23 nested .memql files (dsl/agents/tools/*.memql and
// similar), so the shape is live rather than hypothetical. Two definitions of
// "same domain" were in force at once, and the one the LINT used was the odd
// one out -- which is #2805's unsatisfiable-lint shape again: an author whose
// file is nested could be told by TestNoSameDomainUse (which uses BOOT's rule)
// that an import is forbidden, while the lint's ambient preference (its own
// rule) did not grant it, leaving no spelling that passes both.
//
// It lives in dslfs because dslfs is a LEAF -- zero internal memql deps -- and
// both consumers can import it. `component/memql` imports
// `component/memql/dslimports`, so the obvious fix of having dslimports call
// component/memql's copy is a cycle; #2852 suggested exactly that.

// VersionFromFilePath extracts the legacy version directory from a file path.
//
//	"v1/mutationJoinSpace.memql"                            -> "v1"
//	"mutations/v1/mutationJoinSpace.memql"                  -> "v1"
//	"automations/v1/cognition/autoJoinAI/automation.memql"  -> "v1"
//
// Returns "" when no version directory is present.
func VersionFromFilePath(path string) string {
	for _, part := range strings.Split(path, "/") {
		if len(part) >= 2 && part[0] == 'v' && part[1] >= '0' && part[1] <= '9' {
			allDigits := true
			for _, ch := range part[1:] {
				if ch < '0' || ch > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return part
			}
		}
	}
	return ""
}

// DomainFromFilePath returns the domain directory containing a mounted .memql
// file -- the LAST path segment before the filename that is not a legacy
// v<digits> version directory. This is the file's ambient namespace under the
// #2617 same-domain scope rule ("planner" for planner/queries.memql, "tools"
// for agents/tools/askSpecialist.memql).
//
// Loader origin decorations are tolerated: the unified loader stamps slice
// origins as "unified:<path>:<sliceName>", and the prefix would otherwise
// pollute the leading directory segment.
//
// Returns "" when the path carries no directory.
func DomainFromFilePath(path string) string {
	parts := strings.Split(stripOriginDecoration(path), "/")
	for i := len(parts) - 2; i >= 0; i-- {
		part := parts[i]
		if part == "" || part == "." {
			continue
		}
		if VersionFromFilePath(part) == part {
			continue // legacy v<digits> layout segment, not a domain
		}
		return part
	}
	return ""
}

// RootDomainFromFilePath returns the FIRST directory segment of a mounted
// .memql file's path -- "agents" for agents/tools/askSpecialist.memql, where
// DomainFromFilePath returns "tools".
//
// The two exist side by side because the tree answers two different questions
// with two different segments, and confusing them is a live defect class
// (memql#3026 landing review):
//
//   - the LAST segment is the file's ambient same-domain scope under #2617,
//     which is what DomainFromFilePath is for;
//   - the FIRST segment is the domain a concept's canonical id is ASSEMBLED
//     from. unified_loader.go's pass 1 is `dir := firstPathSegment(p)` before
//     AssembleConceptIdFromDeclInDir, so beta/sub/concepts.memql declares
//     v1:beta:widget, and it is beta -- not sub -- that owns a namespace.pin.
//
// Anything comparing a concept id against a domain wants this one. "" comes
// back when the path carries no directory.
//
// It agrees with firstPathSegment on every path the loader emits, and
// deliberately not on three it does not: a legacy `v<digits>` first segment, a
// leading "./" or "/", and a bare filename. Those are skipped here (matching
// DomainFromFilePath's own table, which treats the legacy version layout as
// live) and returned verbatim there. Loader origin decorations are stripped
// exactly as they are above, which firstPathSegment does not do at all -- so
// "unified:deployment/mutations.memql" resolves here and would yield
// "unified:deployment" there.
func RootDomainFromFilePath(path string) string {
	parts := strings.Split(stripOriginDecoration(path), "/")
	if len(parts) < 2 {
		return "" // no directory component: a bare filename has no domain
	}
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if part == "" || part == "." {
			continue
		}
		if VersionFromFilePath(part) == part {
			continue // legacy v<digits> layout segment, not a domain
		}
		return part
	}
	return ""
}

// stripOriginDecoration removes a loader origin prefix ("unified:", "dryrun:",
// ...): a colon-terminated prefix before the first slash is never part of the
// mounted path. The unified loader stamps slice origins as
// "unified:<path>:<sliceName>", which would otherwise pollute the leading
// directory segment -- and it pollutes it for BOTH readers above, which is why
// this is one function rather than a copy in each.
func stripOriginDecoration(path string) string {
	if idx := strings.Index(path, ":"); idx >= 0 &&
		!strings.Contains(path[:idx], "/") && strings.Contains(path[idx+1:], "/") {
		return path[idx+1:]
	}
	return path
}
