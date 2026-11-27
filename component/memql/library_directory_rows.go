package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// LibraryDirectoryRows is the server-side reduction source for the Library
// directory. It reads one authorized current population in one SQL statement,
// instead of repeatedly enumerating that population for every metadata page.
// It is not a wire-level unbounded query: only these installed, owner-scoped
// reads are accepted, and an excessive population fails rather than silently
// producing incomplete counts or a mis-sorted page.
//
// staged-data: GATE -- use the ordinary parser's staged predicate, surface
// floors and row-authz injection, and admit the actual latest source rows
// before an integration may count or repack them. Never borrow internal origin.
func (e *MemQLEngine) LibraryDirectoryRows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	if actingUserFromContext(ctx) == "" {
		return nil, fmt.Errorf("library directory requires an authenticated caller")
	}
	if name != "libraryArtifactsBrowse" && name != "libraryArtifactFacetRows" && name != "libraryFileSizeRows" {
		return nil, fmt.Errorf("unsupported library directory source")
	}
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	if err := e.refuseUnauthorizedStagedScope(ctx); err != nil {
		return nil, err
	}
	plan, err := e.parseWithFunctionsAmbient("query "+call, e.functions, nil, false, auth.OriginFromContext(ctx), buildAmbientEnvelope(ctx, e), e.stagedScopeFor(ctx))
	if err != nil {
		return nil, err
	}
	if err := e.refusePlanBelowRequiredRank(ctx, plan); err != nil {
		return nil, err
	}
	if err := e.refusePlanBelowRequiredCapability(ctx, plan); err != nil {
		return nil, err
	}
	if err := refuseRowAuthzWithoutActor(ctx, plan); err != nil {
		return nil, err
	}
	ctx = contextWithRowAuthzBinding(ctx, plan.BoundConcept)
	ctx = contextWithRankScopeMemo(ctx, e)
	ctx = contextWithAccountScopeMemo(ctx, e)
	expr, err := resolveActorReferences(ctx, plan.Root)
	if err != nil {
		return nil, err
	}
	expr = e.resolveCanonicalIdComparisons(ctx, expr)
	expr = e.canonicalizeRelationshipComparisons(ctx, expr, plan.BoundConcept)
	expr = e.lowerAccountScope(ctx, e.lowerRankScope(ctx, expr))
	filter, ok := e.tryCompileCombinedFilter(ctx, expr, plan.BoundConcept)
	if !ok {
		return nil, fmt.Errorf("library directory filter cannot be executed in SQL")
	}
	if plan.Refine != nil {
		return nil, fmt.Errorf("library directory source cannot refine a bounded page")
	}
	if plan.BoundConcept != "v1:library:artifact" && plan.BoundConcept != "v1:library:file" {
		return nil, fmt.Errorf("library directory source has an invalid concept")
	}
	// Select only true current rows, including a newer version whose concept,
	// owner or folder changed. A filter on historical rows alone is not enough.
	// The bounded timestamp on the subplan excludes older chunks at execution.
	q := e.database().NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where(filter.sql, filter.args...).
		Where(`NOT EXISTS (SELECT 1 FROM ` + memorynodes.ObservedRowsSQL + ` AS later WHERE later.id = mn.id AND later."createdAt" > mn."createdAt" LIMIT 1 OFFSET 0)`).
		Limit(100001)
	var rows []memorynodes.MemoryNode
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) > 100000 {
		return nil, fmt.Errorf("library directory is too large; narrow the folder or search")
	}
	rows = AdmitSourceRows(ctx, e.filterStagedNodes(ctx, rows))
	bundle, err := e.buildGraphBundle(ctx, rows, 0, nil)
	if err != nil {
		return nil, err
	}
	applyPlanProjection(bundle, plan)
	template := plan.ShapeTemplate
	if plan.ShapeTemplateName != "" {
		template, err = e.resolveNamedShapeForContext(ctx, plan.ShapeTemplateName, plan.SourceFunction)
		if err != nil {
			return nil, err
		}
	}
	var specs map[string]*Spec
	if e.specs != nil {
		specs = e.specs.LookupIndex()
	}
	shaped, err := applyShapeTemplate(ctx, bundle, template, e.aiRuntime, specs)
	if err != nil {
		return nil, err
	}
	return MaterializeRows(shaped), nil
}
