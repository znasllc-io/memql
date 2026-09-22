package procedure

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/planner"
)

// persist.go -- the winner as a construct: rendered, compiled, stored, and
// stamped.
//
// THE ORDER IS THE DESIGN, and one step of it is the whole reason this file
// is separate from learn.go. goalSignature is written LAST, after the compile
// gate has passed. It is the key compile's exact-match tier serves a later
// goal from WITHOUT A MODEL, so a signature written before the construct is
// known to compile points a future goal at a template that cannot run -- and
// the failure surfaces as a run that dies on a template nobody chose, far from
// here. This is recordConstructGoalSignature's first production caller.
//
// NOTHING AUTO-ACTIVATES. The bundle is recorded `validated` and the construct
// stays `draft`; the construct enters D15's ladder as a candidate and a person
// promotes it. A learned procedure that activated itself would replay a
// generalization nobody has read.

// compileEngine is the Gate 1 seam. Optional: a binary without the authoring
// seams linked still stores the procedure as a record, without the verdict.
type compileEngine interface {
	CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport
}

// toolSpeller answers how a tool's call is written as a statement.
type toolSpeller interface {
	ToolCallee(tool string) (kind, callee string, ok bool)
}

// learnAndPersist runs the pipeline and, when an abstraction clears the floor,
// lifts the best one.
func (i *Integration) learnAndPersist(ctx context.Context, k corpusKey) (learnResult, error) {
	res, accepted, corpus, runIds, err := i.runPipeline(ctx, k)
	if err != nil {
		return res, err
	}
	if !res.Accepted || len(accepted) == 0 {
		return res, nil
	}

	win := accepted[0]
	constructId, err := i.lift(ctx, k, win, corpus, runIds)
	if err != nil {
		return res, err
	}
	res.ConstructId = constructId
	return res, nil
}

func (i *Integration) lift(ctx context.Context, k corpusKey, win proc.Accepted, corpus [][]proc.Action, runIds []string) (string, error) {
	owner := strings.TrimSpace(k.OwnerUserId)
	if owner == "" {
		return "", fmt.Errorf("procedure: refusing to lift a procedure with no owner -- the row would be readable by nobody")
	}
	actorCtx := ownerActor(ctx, owner)

	name := procedureName(k)
	prov := planner.TemplateProvenance{
		RunIds: runIdsOf(win, runIds),
		Uses:   win.Utility.Uses,
		Net:    win.Utility.Net,
	}
	var callee planner.ToolCalleeFunc
	if sp, ok := i.store.engine.(toolSpeller); ok {
		callee = sp.ToolCallee
	}
	source, everyStepWritten := planner.RenderTemplateAutomation(name, "", win.Candidate.Template, prov, callee)

	bundleId := id.NewShortId()
	constructId := id.NewShortId()

	if err := i.store.writeInternal(actorCtx, "mutation "+call("createAuthoringBundle", map[string]any{
		"bundleId":    bundleId,
		"title":       fmt.Sprintf("Procedure learned from %d recorded run(s)", win.Utility.Uses),
		"summary":     win.Utility.Reason,
		"sourceRunId": firstRunId(prov.RunIds),
	})); err != nil {
		return "", fmt.Errorf("create bundle: %w", err)
	}

	if err := i.store.writeInternal(actorCtx, "mutation "+call("createAuthoringConstruct", map[string]any{
		"constructId":     constructId,
		"bundleId":        bundleId,
		"kind":            "automation",
		"name":            name,
		"targetNamespace": "procedure",
		"source":          source,
		"origin":          "procedure/automations.memql",
	})); err != nil {
		return "", fmt.Errorf("create construct: %w", err)
	}

	// Gate 1, then -- and only then -- the signature.
	report, gateRan := i.compile(source, name)
	reRunnable := gateRan && report.OK && everyStepWritten
	if err := i.recordValidation(actorCtx, bundleId, report, gateRan, reRunnable); err != nil {
		return "", fmt.Errorf("record validation: %w", err)
	}

	if reRunnable {
		if err := i.store.writeInternal(actorCtx, "mutation "+call("recordConstructGoalSignature", map[string]any{
			"constructId":   constructId,
			"goalSignature": k.GoalSignature,
		})); err != nil {
			return "", fmt.Errorf("record goal signature: %w", err)
		}
	} else {
		i.log().Info("procedure: learned a procedure that is not re-runnable; goalSignature withheld",
			"constructId", constructId, "gateRan", gateRan, "everyStepWritten", everyStepWritten)
	}

	i.log().Info("procedure: lifted",
		"constructId", constructId, "bundleId", bundleId, "name", name,
		"uses", win.Utility.Uses, "net", win.Utility.Net, "reRunnable", reRunnable)
	return constructId, nil
}

func (i *Integration) compile(source, name string) (memql.SandboxReport, bool) {
	ce, ok := i.store.engine.(compileEngine)
	if !ok {
		return memql.SandboxReport{}, false
	}
	return ce.CompileBundle([]memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}}), true
}

func (i *Integration) recordValidation(ctx context.Context, bundleId string, report memql.SandboxReport, gateRan, reRunnable bool) error {
	status := "validated"
	failure := ""
	if gateRan && !report.OK {
		status = "failed"
		failure = "the learned procedure did not compile"
	}
	return i.store.writeInternal(ctx, "mutation "+call("recordBundleValidation", map[string]any{
		"bundleId":      bundleId,
		"status":        status,
		"failureReason": failure,
		"validationReport": map[string]any{
			"gate1Ran":   gateRan,
			"reRunnable": reRunnable,
			"learnedBy":  "component/procedure",
		},
	}))
}

// procedureName is stable for one (owner, signature, level), so re-mining the
// same corpus proposes the same name rather than a new one every sweep.
func procedureName(k corpusKey) string {
	sig := k.GoalSignature
	if len(sig) > 12 {
		sig = sig[:12]
	}
	if sig == "" {
		sig = "unsigned"
	}
	return fmt.Sprintf("learnedProcedure_%s_l%d", sig, int(k.Level))
}

// runIdsOf names the RECORDINGS the procedure was generalized from -- the
// point of D9's stamp. A sequence index would be provenance nobody can follow
// back to anything.
func runIdsOf(win proc.Accepted, runIds []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, occ := range win.Candidate.Occurrences {
		if occ.Sequence >= len(runIds) {
			continue
		}
		rid := runIds[occ.Sequence]
		if rid == "" || seen[rid] {
			continue
		}
		seen[rid] = true
		out = append(out, rid)
	}
	return out
}

func firstRunId(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
