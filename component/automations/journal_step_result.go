package automations

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// journal_step_result.go -- what a step receipt records of an engine result.
//
// A query's rows and the row a mutation wrote reach a step as a Bundle-backed
// *memql.ExecuteResult, and the receipt (MinimalStepResult.Result, the
// v1:work:step row's `result.result`) recorded it whole. On production
// (2026-11-06) a journaled mutation step's receipt was about 5 KB, most of it
// two things nothing reads back:
//
//   - every node's `schema`: the concept's whole JSON schema, which
//     encoding/json writes in protobuf's verbose struct form. It describes the
//     concept, not what the step did, and the stored row it came from already
//     records which schema it used.
//   - the rows themselves when the statement's value was recorded
//     (StepResult.BoundRecorded): MinimalStepResult.Value holds the same nodes
//     in the plain row form (functionStatementValue, bundleNodeMap), and that
//     is the copy a resume rebinds and every reader of a receipt reads first
//     (the OS step presentation, work_follow, the planner's replan evidence,
//     compose's sections).
//
// WHY A RESUME NEEDS NEITHER. A statement body resumes by rebinding each
// finished statement's name to MinimalStepResult.Value (resumedStatements,
// resume_statements.go); Result reaches only the resumed execution's record of
// the steps it did not run again (minimalToStepResult), which nothing reads
// for a value. A query whose rows were too many to record as a value is read
// again on resume (reread), never served from Result. No reader of a receipt
// reads a node's schema. TestResumeV1_DB_AMutationReceiptHoldsItsRowOnceAndNoSchema
// resumes over a real mutation's receipt to hold that.
//
// WHAT STAYS. A Bundle-backed result whose value was NOT recorded -- a
// statement that binds no name -- keeps its nodes, edges, root ids and meta in
// the shape it always had, minus each node's schema: it is the only record of
// what the step returned. Every other result -- a logic's or a builtin's flat
// output, an action's record -- is recorded exactly as before: the validator
// and the replan prompt read those whole. And only the journal's copy changes;
// the live StepResult and the execution's in-memory steps keep the full
// result.
//
// The resultFingerprint is unchanged: StepDeterministicFingerprint is taken
// from the live result before the receipt is trimmed.

// journaledResult is the copy of a step's (unwrapped) result its receipt
// records. valueRecorded is StepResult.BoundRecorded.
func journaledResult(v any, valueRecorded bool) any {
	er, ok := v.(*memql.ExecuteResult)
	if !ok || er == nil || er.Bundle == nil {
		return v
	}
	if valueRecorded {
		return nil
	}
	return &memql.ExecuteResult{Bundle: bundleWithoutSchemas(er.Bundle), Meta: er.Meta}
}

// bundleWithoutSchemas copies a bundle leaving every node's schema out and
// sharing everything else with the original, which nothing here mutates.
//
// A node is copied field by field through protoreflect rather than by naming
// its fields, so a field MemoryNode gains later reaches the receipt rather than
// being dropped by a list nobody remembered to extend.
// TestBundleWithoutSchemasKeepsEveryOtherNodeField holds that.
func bundleWithoutSchemas(b *memqlv1.GraphBundle) *memqlv1.GraphBundle {
	out := &memqlv1.GraphBundle{
		Edges:   b.GetEdges(),
		RootIds: b.GetRootIds(),
		Nodes:   make([]*memqlv1.MemoryNode, 0, len(b.GetNodes())),
	}
	for _, n := range b.GetNodes() {
		if n == nil {
			out.Nodes = append(out.Nodes, nil)
			continue
		}
		c := &memqlv1.MemoryNode{}
		dst := c.ProtoReflect()
		n.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.Name() != nodeSchemaField {
				dst.Set(fd, v)
			}
			return true
		})
		out.Nodes = append(out.Nodes, c)
	}
	return out
}

// nodeSchemaField is MemoryNode's schema field, by its proto name.
const nodeSchemaField protoreflect.Name = "schema"
