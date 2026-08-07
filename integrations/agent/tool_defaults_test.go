package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/core/common"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// tool_defaults_test.go -- memql#3237.
//
// The defect had two halves and the tests below take them in that order.
//
//	DELIVERY: this package never called common.ContextWithToolDefaults, so
//	ExecuteTool's applyToolDefaults ran with an empty defaults map, deleted
//	every @autoInjected field, and put nothing back. The value
//	injectAgentContext had just stamped was gone by the time the handler saw
//	the args.
//
//	COVERAGE: agentContextStamps -- the table that decides what the runtime
//	stamps -- had no entry for editDocument at all, whose three
//	@autoInjected fields were therefore never stamped in the first place and
//	then stripped anyway.
//
// A test for either half alone passes against a fix for the other, so both are
// pinned, and the coverage gate reads the REAL DSL tree rather than a list
// written here -- a list would agree with itself forever, which is exactly how
// editDocument stayed missing.

// turnCtxAllFields is a fully-resolved turn: every field the runtime can stamp
// has a value. Used by the coverage gate, where "the runtime resolved nothing
// this turn" and "the table has no entry for this tool" must not look alike.
func turnCtxAllFields() turnContext {
	return turnContext{
		AgentId:     "v1:agents:agent:runtime",
		OwnerUserId: "v1:identity:user:owner",
		PartitionId: "v1:cognition:space:s1",
		PlanId:      "v1:planner:plan:p1",
	}
}

// autoInjectedFieldsByTool parses the embedded DSL tree and returns, per tool,
// the field names declared @autoInjected.
//
// Read from the tree rather than restated here on purpose. A hardcoded list is
// satisfied by itself: it cannot notice a new tool, and it did not notice
// editDocument.
func autoInjectedFieldsByTool(t *testing.T) map[string][]string {
	t.Helper()
	tree, err := dslimports.Load(memqldsl.Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v -- the DSL tree did not parse, so this gate cannot "+
			"derive the @autoInjected set. Fix the parse failure; do not read this as "+
			"'no tool declares one'.", err)
	}
	out := map[string][]string{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			decl, ok := def.(*languageAst.ToolDecl)
			if !ok || decl.Disabled {
				continue
			}
			for _, f := range decl.Fields {
				if f.AutoInjected {
					out[decl.Name] = append(out[decl.Name], f.Name)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("walked the DSL tree and found ZERO @autoInjected fields. Either the " +
			"annotation has no live users -- in which case check whether the strip in " +
			"component/memql/tool_args.go is exercised by anything -- or the AST field " +
			"changed, and this gate now passes vacuously over nothing.")
	}
	return out
}

// THE COVERAGE HALF. For every tool the agent runtime dispatches with runtime
// context, every field that tool declares @autoInjected must have a default the
// runtime can supply.
//
// The consequence of a gap is not "the value is missing" -- it is worse than
// that. applyToolDefaults deletes an @autoInjected field with no default, so an
// uncovered field is deleted at dispatch on EVERY call, and the handler sees it
// as absent no matter what anyone supplies. The tool fails closed, permanently
// and silently.
func TestAgentToolDefaultsCoverEveryAutoInjectedField(t *testing.T) {
	declared := autoInjectedFieldsByTool(t)
	turnCtx := turnCtxAllFields()

	for toolName, fields := range declared {
		stamp, dispatched := agentContextStamps[toolName]
		if !dispatched {
			// Not a runtime-context tool as far as this package is concerned.
			// That is a claim worth failing on when the tool declares
			// @autoInjected fields: something must supply them, and if it is
			// not this table then the fields are stripped on every agent
			// dispatch of that tool.
			t.Errorf("tool %q declares @autoInjected %v but has no agentContextStamps entry.\n\n"+
				"applyToolDefaults DELETES an @autoInjected field when the runtime supplies no "+
				"default, so on the agent path every one of those fields is dropped at dispatch "+
				"-- the handler sees them as absent whatever the model sent, and the tool fails "+
				"closed on every call. Add the entry (and its flags) rather than removing the "+
				"annotation: the annotation is what stops the model forging the value. "+
				"memql#3237.", toolName, fields)
			continue
		}
		_ = stamp

		supplied := agentToolDefaults(toolName, turnCtx)
		var missing []string
		for _, f := range fields {
			if _, ok := supplied[f]; !ok {
				missing = append(missing, f)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("tool %q declares @autoInjected %v; the runtime supplies %v -- missing %v.\n\n"+
				"Each missing field is deleted at dispatch with nothing put back. memql#3237.",
				toolName, fields, keysOf(supplied), missing)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// THE DELIVERY HALF, and the one that is RED against untouched main: the
// dispatch context must carry the defaults, because that is the only channel
// applyToolDefaults reads. Before this change the four dispatch sites in this
// package passed a bare ctx.
func TestAgentToolCallContextDeliversTheStamp(t *testing.T) {
	turnCtx := turnCtxAllFields()

	for _, tc := range []struct {
		tool string
		want map[string]string
	}{
		{"produceArtifact", map[string]string{
			"agentId":     "v1:agents:agent:runtime",
			"ownerUserId": "v1:identity:user:owner",
			"partitionId": "v1:cognition:space:s1",
		}},
		{"requestUserFeedback", map[string]string{
			"agentId":     "v1:agents:agent:runtime",
			"ownerUserId": "v1:identity:user:owner",
			"partitionId": "v1:cognition:space:s1",
			"planId":      "v1:planner:plan:p1",
		}},
		{"editDocument", map[string]string{
			"agentId":          "v1:agents:agent:runtime",
			"producedByPlanId": "v1:planner:plan:p1",
			"partitionId":      "v1:cognition:space:s1",
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			ctx := agentToolCallContext(t.Context(), tc.tool, turnCtx)
			got := common.ToolDefaultsFromContext(ctx)
			if len(got) == 0 {
				t.Fatalf("the dispatch context for %q carries NO tool defaults.\n\n"+
					"applyToolDefaults reads them from the context and from nowhere else, so "+
					"every @autoInjected field on this tool is deleted at dispatch with nothing "+
					"put back -- which is memql#3237 exactly. This package had no "+
					"ContextWithToolDefaults caller at all.", tc.tool)
			}
			for field, want := range tc.want {
				if got[field] != want {
					t.Errorf("default %q = %v, want %q", field, got[field], want)
				}
			}
		})
	}
}

// A tool with no runtime context must leave the context alone rather than
// acquire an empty defaults map -- the presence of the map is itself a signal
// applyToolDefaults branches on.
func TestAgentToolCallContextIsANoOpForAnUncontextedTool(t *testing.T) {
	ctx := t.Context()
	if got := agentToolCallContext(ctx, "searchUsers", turnCtxAllFields()); got != ctx {
		t.Error("a tool with no agentContextStamps entry acquired a defaults context")
	}
}

// An UNRESOLVED turn must supply no default rather than an empty-string one.
// This is the difference between "the field is absent and the handler says so"
// and "the field is present and wrong": a default of "" is RESTORED over the
// anti-forgery strip and reaches the handler as a real value, so a query
// filtering `ownerUserId == ""` matches nothing and reports it as "no rows"
// rather than as a missing actor.
func TestAgentToolDefaultsOmitUnresolvedFields(t *testing.T) {
	// A chat-driven turn: an agent, but no plan and no owner resolved.
	partial := turnContext{AgentId: "v1:agents:agent:runtime"}

	got := agentToolDefaults("requestUserFeedback", partial)
	if _, present := got["planId"]; present {
		t.Errorf("planId defaulted to %v on a turn that resolved none", got["planId"])
	}
	if _, present := got["ownerUserId"]; present {
		t.Errorf("ownerUserId defaulted to %v on a turn that resolved none", got["ownerUserId"])
	}
	if got["agentId"] != "v1:agents:agent:runtime" {
		t.Errorf("agentId = %v, want the resolved value", got["agentId"])
	}
}

// bareDispatch matches a tool dispatch that passes the RAW turn context rather
// than the per-call defaults context. The fix is four call sites today; this is
// what stops a fifth being added without one, which is how the delivery gap
// would come back on exactly one path and be invisible on the others.
var bareDispatch = regexp.MustCompile(`ExecuteToolByName\(\s*ctx\s*,`)

func TestEveryAgentToolDispatchCarriesTheDefaultsContext(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(src)
		if strings.Contains(text, "ExecuteToolByName(agentToolCallContext(") {
			found += strings.Count(text, "ExecuteToolByName(agentToolCallContext(")
		}
		if loc := bareDispatch.FindStringIndex(text); loc != nil {
			line := 1 + strings.Count(text[:loc[0]], "\n")
			t.Errorf("%s:%d dispatches a tool with the bare turn context.\n\n"+
				"applyToolDefaults reads the server defaults off the context and from nowhere "+
				"else, so a dispatch that does not wrap it in agentToolCallContext strips every "+
				"@autoInjected field on that tool and puts nothing back. Wrap it. memql#3237.",
				name, line)
		}
	}
	if found == 0 {
		t.Error("found no agentToolCallContext-wrapped dispatch in this package. Either the " +
			"dispatch moved, in which case this gate now passes vacuously, or the delivery " +
			"was removed.")
	}
}
