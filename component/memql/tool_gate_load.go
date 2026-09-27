package memql

// tool_gate_load.go -- the LOAD-time half of the tool gates (memql#5438).
//
// A gate whose value names nothing is worse than no gate: it reads like a
// restriction while deciding nothing a reader expects. Each of a tool's two
// gates is therefore checked when the tree loads, and a value that cannot be
// held refuses a strict boot (MEMQL_DSL_ALLOW_SKIPS is the break-glass) with
// the construct, where it is written and what to write instead:
//
//   - @requiresAgentRole's values are checked against the v1:agents:agent
//     concept's OWN `role` enum, read from the concept registry this load
//     built -- the vocabulary is the concept's declaration, not a list kept
//     beside it, so the gate and the column an agent's role is written in
//     cannot disagree. A value no agent can hold would admit no agent through
//     it.
//   - @requiresRank's slug is checked against the role ladder exactly as a
//     query's, mutation's or logic's is (validateRequiresRankSlugs): a floor
//     that does not resolve is refused, for the reason given there.
//
// Both refusals carry a stable rule id, printed last in brackets like every
// coded load refusal, which is what the conformance corpus keys on.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// The tool gates' load refusal codes.
const (
	// RuleRequiresAgentRoleUnknown: @requiresAgentRole names a value that is
	// not in v1:agents:agent's role enum.
	RuleRequiresAgentRoleUnknown = "requires_agent_role_unknown"
	// RuleRequiresRankUnknown: @requiresRank names no role in the ladder --
	// on a tool, a query, a mutation or a logic alike.
	RuleRequiresRankUnknown = "requires_rank_unknown"
)

// agentRoleField is the field of conceptAgentsAgent whose enum is the
// agent-role vocabulary @requiresAgentRole is written in.
const agentRoleField = "role"

// gateLoadRefusal is a coded load refusal: its text ends with the rule id in
// brackets, and RuleCode reports it to the load report
// (baseloader.CodedRefusal).
type gateLoadRefusal struct {
	code, message string
}

func (r *gateLoadRefusal) Error() string    { return r.message + " [" + r.code + "]" }
func (r *gateLoadRefusal) RuleCode() string { return r.code }

// toolGateProblem is one refused tool gate: the tool, the file it is written
// in, and the refusal.
type toolGateProblem struct {
	tool string
	file string
	err  error
}

// agentRoleVocabulary reads the agent-role enum from the v1:agents:agent
// declaration in concepts. ok is false when the concept, or its role field's
// enum, is not there: then nothing can be checked, and the caller refuses
// every value rather than admitting them unchecked.
func agentRoleVocabulary(concepts concept.Registry) (roles []string, ok bool) {
	if concepts == nil {
		return nil, false
	}
	c, err := concepts.Get(conceptAgentsAgent)
	if err != nil || c == nil {
		return nil, false
	}
	fields, err := flattenConceptFields(c)
	if err != nil {
		return nil, false
	}
	role, present := fields[agentRoleField]
	if !present || len(role.Enum) == 0 {
		return nil, false
	}
	return append([]string(nil), role.Enum...), true
}

// validateToolGateDeclarations checks every tool's @requiresAgentRole values
// against the agent-role enum and its @requiresRank slug against the role
// ladder, and returns one problem per refused gate, sorted by tool, so a boot
// failure names the same construct first every time.
func (e *MemQLEngine) validateToolGateDeclarations(ctx context.Context, tools *ToolRegistry, concepts concept.Registry, raw []baseloader.RawFile) []toolGateProblem {
	if tools == nil {
		return nil
	}
	// List, not LookupIndex: the index also carries each tool under its bare
	// alias, and a tool is one declaration however many names reach it.
	var gated []*Tool
	for _, t := range tools.List() {
		if t != nil && (len(t.RequiresAgentRole) > 0 || strings.TrimSpace(t.RequiresRank) != "") {
			gated = append(gated, t)
		}
	}
	if len(gated) == 0 {
		return nil
	}
	sort.SliceStable(gated, func(i, j int) bool { return gated[i].Name < gated[j].Name })

	sources := make(map[string]string, len(raw))
	for _, f := range raw {
		sources[f.Path] = f.Content
	}
	vocabulary, vocabularyOK := agentRoleVocabulary(concepts)
	var ladder roleLadder
	ladderRead := false

	var problems []toolGateProblem
	for _, t := range gated {
		file := unifiedOriginPath(t.Origin, t.Name)
		if len(t.RequiresAgentRole) > 0 {
			at := toolAnnotationPosition(sources[file], file, t.Name, "requiresAgentRole")
			if !vocabularyOK {
				problems = append(problems, toolGateProblem{tool: t.Name, file: file, err: &gateLoadRefusal{
					code: RuleRequiresAgentRoleUnknown,
					message: fmt.Sprintf("tool %q (%s) declares @requiresAgentRole(%s), and its vocabulary -- the %q enum of %s -- is not loaded, "+
						"so no value can be checked; a gate that cannot be checked is refused rather than trusted",
						t.Name, at, quotedList(t.RequiresAgentRole), agentRoleField, conceptAgentsAgent),
				}})
			} else {
				for _, v := range t.RequiresAgentRole {
					if containsExact(vocabulary, v) {
						continue
					}
					problems = append(problems, toolGateProblem{tool: t.Name, file: file, err: &gateLoadRefusal{
						code: RuleRequiresAgentRoleUnknown,
						message: fmt.Sprintf("tool %q (%s) declares @requiresAgentRole(%s), and %q is not an agent role: %s.%s is one of %s. "+
							"A value no agent can hold admits no agent through it, so this refuses to load -- write one of those, "+
							"or @requiresRank(\"<role>\") if the gate is on the person the call is for rather than on the agent",
							t.Name, at, quotedList(t.RequiresAgentRole), v, conceptAgentsAgent, agentRoleField, strings.Join(vocabulary, ", ")),
					}})
					break
				}
			}
		}
		if slug := strings.TrimSpace(t.RequiresRank); slug != "" {
			if !ladderRead {
				ladder, ladderRead = e.rankLadder(ctx), true
			}
			if ladder.rankOf(slug) > 0 {
				continue
			}
			at := toolAnnotationPosition(sources[file], file, t.Name, "requiresRank")
			problems = append(problems, toolGateProblem{tool: t.Name, file: file, err: &gateLoadRefusal{
				code: RuleRequiresRankUnknown,
				message: fmt.Sprintf("tool %q (%s) declares @requiresRank(%q), which names no role in dsl/rbac. "+
					"A floor that does not resolve ranks 0 and would admit every caller, so this refuses to load. Known roles: %s",
					t.Name, at, slug, strings.Join(ladder.knownSlugs(), ", ")),
			}})
		}
	}
	return problems
}

// recordToolGateProblems runs validateToolGateDeclarations and folds each
// problem onto the load report as a coded Skip, so strict boot refuses it the
// way it refuses a construct that failed to parse.
func (e *MemQLEngine) recordToolGateProblems(ctx context.Context, report *LoadReport, tools *ToolRegistry, concepts concept.Registry, raw []baseloader.RawFile) []toolGateProblem {
	problems := e.validateToolGateDeclarations(ctx, tools, concepts, raw)
	for _, p := range problems {
		report.AddSkip(baseloader.SkipFor("memql.toolGates", "tool", p.tool, p.file, "contract-gate:toolGates", p.err))
	}
	return problems
}

// toolAnnotationPosition renders where a tool's annotation is written, as
// "<file>:<line>": the line of the `@<annotation>(` in the tool's annotation
// run, or the tool's own header line when the run cannot be found in src (a
// tool built in Go, or an origin that does not name a file this load read).
func toolAnnotationPosition(src, file, tool, annotation string) string {
	if strings.TrimSpace(file) == "" {
		return "no origin recorded"
	}
	header := regexp.MustCompile(`^\s*tool\s+` + regexp.QuoteMeta(tool) + `\b`)
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if !header.MatchString(line) {
			continue
		}
		// Walk up the annotation run above the header: annotation, comment
		// and blank lines belong to it; anything else ends it.
		for j := i - 1; j >= 0; j-- {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "@"+annotation+"(") {
				return fmt.Sprintf("%s:%d", file, j+1)
			}
			if trimmed != "" && !strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "//") {
				break
			}
		}
		return fmt.Sprintf("%s:%d", file, i+1)
	}
	return file
}

// quotedList renders values as they are written in an annotation:
// "a", "b".
func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return strings.Join(quoted, ", ")
}
