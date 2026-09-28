package bundle

import (
	"fmt"
	"sort"
)

// The boundary rules. The link rules can be allowlisted by (file, target,
// rule); the page rules cannot, because an allowlist entry names a link.
const (
	// RuleLeavesPublic: a relative link resolves outside docs/public -- into
	// docs/internal, docs/superpowers, or the source tree. Nothing there is on
	// the site, and the design records and internal docs are not public.
	RuleLeavesPublic = "leaves-public"
	// RuleUnselectedTarget: a relative link names a docs/public page the
	// bundle does not publish (a draft, a historical page, an internal one).
	RuleUnselectedTarget = "unselected-target"
	// RuleUnrewritableTarget: a relative link names a docs/public path that is
	// not a page -- a directory, a JSON file, an image -- which has no route.
	RuleUnrewritableTarget = "unrewritable-target"
	// RuleMissingTarget: a relative link names nothing tracked under
	// docs/public at all.
	RuleMissingTarget = "missing-target"

	// RuleInstanceExposure: a public page marked `exposure: instance`.
	RuleInstanceExposure = "instance-exposure"
	// RuleUnknownExposure: an `exposure` value outside engine | instance.
	RuleUnknownExposure = "unknown-exposure"
	// RuleUnknownArea: a published page whose area is not in Areas.
	RuleUnknownArea = "unknown-area"
	// RuleRouteCollision: two pages served at one URL.
	RuleRouteCollision = "route-collision"
	// RuleStaleAllowlistEntry: an allowlist entry that matches no link, so the
	// list only ever shrinks.
	RuleStaleAllowlistEntry = "stale-allowlist-entry"
)

// linkRules are the rules an allowlist entry may name.
var linkRules = map[string]bool{
	RuleLeavesPublic:       true,
	RuleUnselectedTarget:   true,
	RuleUnrewritableTarget: true,
	RuleMissingTarget:      true,
}

// Violation is one breach of the public boundary.
type Violation struct {
	File     string `json:"file"`           // repository-relative
	Line     int    `json:"line,omitempty"` // 1-based, 0 for a page-level rule
	Target   string `json:"target,omitempty"`
	Resolved string `json:"resolved,omitempty"` // where the target lands, repository-relative
	Rule     string `json:"rule"`
	Detail   string `json:"detail,omitempty"`
}

func (v Violation) String() string {
	loc := v.File
	if v.Line > 0 {
		loc = fmt.Sprintf("%s:%d", v.File, v.Line)
	}
	s := loc + ": " + v.Rule
	if v.Target != "" {
		s += ": " + v.Target
		if v.Resolved != "" && v.Resolved != v.Target {
			s += " (resolves to " + v.Resolved + ")"
		}
	}
	if v.Detail != "" {
		s += " -- " + v.Detail
	}
	return s
}

func sortViolations(vs []Violation) {
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].File != vs[j].File {
			return vs[i].File < vs[j].File
		}
		if vs[i].Line != vs[j].Line {
			return vs[i].Line < vs[j].Line
		}
		return vs[i].Target < vs[j].Target
	})
}
