// Package surfaceresolver implements Phase 2 of the action library (#1737,
// epic #1734): binding an abstract capability to a concrete execution surface
// at replay time, and the resource-coupling correctness constraint (decision
// A) that keeps coupled actions in one world.
//
// It is pure: the harness supplies the registered surfaces (from
// v1:actions:surface) plus the action's calls + resource edges, and this
// package answers "which surface should each call run on" with the precedence
//
//	explicit binding  >  policy default  >  availability failover
//
// and the decision-A guarantee that every call in a resource-coupled world
// group resolves to a SINGLE surface. Where a replay would be forced to cross
// worlds, it reports the crossing so the caller can insert an explicit
// transfer action rather than silently reading from the wrong filesystem.
package surfaceresolver

import (
	"fmt"
	"sort"
	"strings"
)

// Surface is a registered execution backend (a projection of
// v1:actions:surface). Capabilities is the set of abstract verbs it serves;
// Available + Priority drive the failover chain (lower Priority first).
type Surface struct {
	ID           string
	Slug         string
	Kind         string // workbench | computer-use | mcp
	MachineID    string // for computer-use:<machineId>
	Capabilities []string
	Available    bool
	Priority     int
}

func (s Surface) serves(capability string) bool {
	for _, c := range s.Capabilities {
		if capabilityMatches(c, capability) {
			return true
		}
	}
	return false
}

// capabilityMatches reports whether a surface's declared capability pattern
// covers a concrete capability verb. A surface declares coverage as DATA
// (action-library §7), so a pattern may be:
//   - exact ("fs.writeFile") -- matches that verb only;
//   - a namespace wildcard ("fs.*") -- matches any verb under that namespace
//     ("fs.writeFile", "fs.readFile", "integration.argocd.sync" under
//     "integration.argocd.*", ...). The "." is kept so "fs.*" matches
//     "fs.writeFile" but NOT a sibling namespace like "fsx.y";
//   - "*" -- matches every capability.
func capabilityMatches(pattern, capability string) bool {
	if pattern == capability || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-1] // drop the "*", keep the trailing "."
		return strings.HasPrefix(capability, prefix)
	}
	return false
}

// Call is one capability call to resolve a surface for.
type Call struct {
	Index      int
	Capability string
}

// ResourceEdge couples a producer call to a consumer call that reads the same
// resource (from the Phase 0 tracer). Coupled calls form a world group.
type ResourceEdge struct {
	FromCallIndex int
	ToCallIndex   int
	Resource      string
}

// Resolution is the surface chosen for a single call, with the world group it
// belongs to (calls sharing a group resolve to the same surface, decision A).
type Resolution struct {
	CallIndex int
	Capability string
	Surface   Surface
	GroupID   int
}

// Plan is the full resolution for an action's calls plus any forced
// cross-world crossings that need an explicit transfer action.
type Plan struct {
	Resolutions []Resolution
	Transfers   []Transfer
}

// Transfer marks a forced cross-world hop: the calls on either side resolved
// to different surfaces because no single available surface served the whole
// coupled group, so a transfer action must bridge them.
type Transfer struct {
	Resource    string
	FromSurface Surface
	ToSurface   Surface
	FromCall    int
	ToCall      int
}

// Resolve binds one capability to a surface with the documented precedence.
// explicit and policyDefault are surface slugs ("" = unset). It returns an
// error when nothing available serves the capability.
func Resolve(capability, explicit, policyDefault string, surfaces []Surface) (Surface, error) {
	avail := availableServing(capability, surfaces)
	if explicit != "" {
		for _, s := range avail {
			if s.Slug == explicit {
				return s, nil
			}
		}
		return Surface{}, fmt.Errorf("surfaceresolver: explicit surface %q does not serve %q or is unavailable", explicit, capability)
	}
	if policyDefault != "" {
		for _, s := range avail {
			if s.Slug == policyDefault {
				return s, nil
			}
		}
	}
	if len(avail) == 0 {
		return Surface{}, fmt.Errorf("surfaceresolver: no available surface serves capability %q", capability)
	}
	return avail[0], nil // availability failover: lowest Priority first
}

// availableServing returns the available surfaces serving capability, ordered
// by Priority (ascending), then Slug for determinism.
func availableServing(capability string, surfaces []Surface) []Surface {
	var out []Surface
	for _, s := range surfaces {
		if s.Available && s.serves(capability) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// WorldGroups partitions call indices [0,numCalls) into resource-coupled
// groups via union-find over the edges. Calls with no edge are singletons.
// Returns a groupID per call index.
func WorldGroups(numCalls int, edges []ResourceEdge) []int {
	parent := make([]int, numCalls)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			// Keep the lower root for stable group ids.
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	for _, e := range edges {
		if e.FromCallIndex >= 0 && e.FromCallIndex < numCalls && e.ToCallIndex >= 0 && e.ToCallIndex < numCalls {
			union(e.FromCallIndex, e.ToCallIndex)
		}
	}
	// Normalize roots to dense group ids by first appearance.
	groupOf := make([]int, numCalls)
	idByRoot := map[int]int{}
	next := 0
	for i := 0; i < numCalls; i++ {
		r := find(i)
		gid, ok := idByRoot[r]
		if !ok {
			gid = next
			idByRoot[r] = gid
			next++
		}
		groupOf[i] = gid
	}
	return groupOf
}

// ResolvePlan resolves a surface for every call, enforcing decision A: all
// calls in a world group resolve to the SAME surface -- the resolver picks one
// available surface that serves EVERY capability in the group (precedence:
// explicit > policy > availability). If no single surface can serve the whole
// group, the group is split across surfaces by best-effort per-call resolution
// and the forced crossings are reported as Transfers (the caller inserts an
// explicit transfer action; there is never a silent cross-world read).
func ResolvePlan(calls []Call, edges []ResourceEdge, explicit, policyDefault string, surfaces []Surface) (Plan, error) {
	groupOf := WorldGroups(len(calls), edges)
	// Collect capabilities per group.
	groupCaps := map[int][]string{}
	groupCallIdx := map[int][]int{}
	for i, c := range calls {
		g := groupOf[i]
		groupCaps[g] = append(groupCaps[g], c.Capability)
		groupCallIdx[g] = append(groupCallIdx[g], i)
	}

	plan := Plan{Resolutions: make([]Resolution, len(calls))}
	groupSurface := map[int]Surface{}
	for g, caps := range groupCaps {
		s, err := resolveGroup(caps, explicit, policyDefault, surfaces)
		if err == nil {
			groupSurface[g] = s
			continue
		}
		// No single surface serves the whole group -> split per call and
		// record the forced crossings along this group's resource edges.
		for _, idx := range groupCallIdx[g] {
			per, perErr := Resolve(calls[idx].Capability, explicit, policyDefault, surfaces)
			if perErr != nil {
				return Plan{}, fmt.Errorf("surfaceresolver: group %d call %d (%s): %w", g, idx, calls[idx].Capability, perErr)
			}
			plan.Resolutions[idx] = Resolution{CallIndex: idx, Capability: calls[idx].Capability, Surface: per, GroupID: g}
		}
	}
	// Fill resolutions for groups that resolved as a whole.
	for i, c := range calls {
		g := groupOf[i]
		if s, ok := groupSurface[g]; ok {
			plan.Resolutions[i] = Resolution{CallIndex: i, Capability: c.Capability, Surface: s, GroupID: g}
		}
	}
	// Detect forced crossings: an edge whose endpoints resolved to different
	// surfaces requires a transfer action.
	for _, e := range edges {
		if e.FromCallIndex < 0 || e.FromCallIndex >= len(calls) || e.ToCallIndex < 0 || e.ToCallIndex >= len(calls) {
			continue
		}
		from := plan.Resolutions[e.FromCallIndex].Surface
		to := plan.Resolutions[e.ToCallIndex].Surface
		if from.ID != to.ID {
			plan.Transfers = append(plan.Transfers, Transfer{
				Resource:    e.Resource,
				FromSurface: from,
				ToSurface:   to,
				FromCall:    e.FromCallIndex,
				ToCall:      e.ToCallIndex,
			})
		}
	}
	return plan, nil
}

// resolveGroup picks one available surface serving EVERY capability in the
// group, honoring precedence (explicit > policy > availability/priority).
func resolveGroup(caps []string, explicit, policyDefault string, surfaces []Surface) (Surface, error) {
	servesAll := func(s Surface) bool {
		if !s.Available {
			return false
		}
		for _, c := range caps {
			if !s.serves(c) {
				return false
			}
		}
		return true
	}
	if explicit != "" {
		for _, s := range surfaces {
			if s.Slug == explicit && servesAll(s) {
				return s, nil
			}
		}
		return Surface{}, fmt.Errorf("explicit surface %q cannot serve the whole world group", explicit)
	}
	candidates := make([]Surface, 0, len(surfaces))
	for _, s := range surfaces {
		if servesAll(s) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return Surface{}, fmt.Errorf("no single surface serves the whole world group %v", caps)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		// policy default wins, then priority, then slug.
		pi := candidates[i].Slug == policyDefault
		pj := candidates[j].Slug == policyDefault
		if pi != pj {
			return pi
		}
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		return candidates[i].Slug < candidates[j].Slug
	})
	return candidates[0], nil
}
