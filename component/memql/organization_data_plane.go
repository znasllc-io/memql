package memql

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/component/auth"
)

// OrganizationDataPlaneEnforced classifies requests whose complete data access
// is enforced by target organization guards. It grants nothing. Unknown forms,
// mixed reads/writes, logic, traversals, and projections of related data retain
// the transport's global gate. Parsing uses the same registry and actor as Execute.
func (e *MemQLEngine) OrganizationDataPlaneEnforced(ctx context.Context, query string) bool {
	if e == nil {
		return false
	}
	plan, err := e.parseWithFunctionsAmbient(query, e.functions, nil, false, auth.OriginFromContext(ctx), buildAmbientEnvelope(ctx, e), StagedScope{})
	if err != nil || plan == nil || plan.LogicCall != nil || len(plan.Relationships) > 0 || plan.Refine != nil || plan.IncludeBundle || (plan.Depth != nil && *plan.Depth > 1) {
		return false
	}
	if plan.MutationCall != nil {
		if plan.Root != nil || len(plan.Mutations) > 0 {
			return false
		}
		fn, ok := e.functions.Lookup(plan.MutationCall.Name)
		// Mutation values have already passed the language's pure-value checker.
		return ok && fn.FunctionKind == "mutation" && fn.MutationTemplate != nil && organizationDataWriteConcept(fn.MutationTemplate.Concept) && organizationPlainValues(plan.MutationCall.Args)
	}
	if len(plan.Mutations) > 0 {
		if plan.Root != nil {
			return false
		}
		for _, m := range plan.Mutations {
			if !organizationDataWriteConcept(m.Concept) || !json.Valid([]byte(m.PayloadRaw)) {
				return false
			}
		}
		return true
	}
	if builtin, ok := plan.Root.(*BuiltinFunctionExpression); ok {
		if !organizationPlainValues(builtin.Args) {
			return false
		}
		// The construct the call resolved to, not its bare name: a name the
		// classifier cannot resolve to exactly one construct keeps the global
		// gate.
		fn, _, err := e.resolveBuiltinConstruct(builtin)
		if err != nil || fn.FunctionKind != "builtin" {
			return false
		}
		if organizationCollectionRead(fn, builtin.Args) {
			return true
		}
		if personalSourceBuiltin(fn) {
			verb, resource := personalSourceRequirement(fn.Name)
			return e.personalSourceCapable(ctx, verb, resource)
		}
		// This exact builtin exposes only the verified caller's own decisions.
		switch fn.Name {
		case "effectiveCapabilitiesForActor":
			return true
		// These management doors authorize the persisted target organization
		// using create/update on group in integrations/groups before writing
		// with synthetic store authority. Self-removal is intentionally free.
		// They must not require a scoped role to hold targetless data authority.
		case "groupCreate", "groupUpdate", "groupArchive", "groupMemberAdd", "groupMemberRemove", "groupPeople":
			return true
		}
		concept, _ := organizationCapabilityTarget(fn)
		return concept != ""
	}
	personalRead := false
	if fn, ok := e.functions.Lookup(plan.SourceFunction); ok && personalSourceQuery(fn) {
		verb, resource := personalSourceRequirement(fn.Name)
		personalRead = e.personalSourceCapable(ctx, verb, resource)
	}

	if plan.SourceFunction == "" || (!HasOrganizationBoundary(plan.BoundConcept) && !personalRead) || !e.organizationPlainFilter(plan.Root, map[string]bool{}) {
		return false
	}
	if plan.Count {
		return true
	}
	// A graph bundle expands related, possibly personal data by default. Only
	// root-only shapes are safe to defer; merely selecting fields is not enough.
	shape := plan.ShapeTemplate
	if shape == nil && plan.ShapeTemplateName != "" {
		shape, err = e.resolveNamedShapeForContext(ctx, plan.ShapeTemplateName, plan.SourceFunction)
		if err != nil {
			return false
		}
	}
	return organizationRootShape(shape)
}

// These collection readers resolve every row under the caller before
// reducing it. They have no single target for organizationCapabilityTarget:
// an omitted account means all readable organizations, never all data. A
// scoped role must reach those guards without gaining targetless data read.
// Match both construct and executor so a same-named authored builtin cannot
// borrow this exception. The directory's sources mode also reads personal
// credential provenance, so it deliberately retains the global gate.
func organizationCollectionRead(fn *Function, args map[string]any) bool {
	if fn == nil || fn.FunctionKind != "builtin" {
		return false
	}
	switch {
	case fn.Name == "deployableDirectoryPage" && fn.Executor == "integration.packages.directoryPage":
		resource, present := args["resource"]
		if !present {
			return true
		}
		value, ok := resource.(string)
		return ok && (value == "" || value == "deployables")
	case fn.Name == "siteStatistics" && fn.Executor == "integration.siteTraffic.statistics":
		return true
	case fn.Name == "emailStatistics" && fn.Executor == "integration.emailStats.statistics":
		return true
	}
	return false
}

func organizationDataWriteConcept(concept string) bool {
	return organizationOwnedConcept(concept) && concept != conceptIdentityGroup && concept != conceptIdentityGroupMembership
}

// Arguments here are values, never expression nodes to execute later.
func organizationPlainValues(v any) bool {
	switch n := v.(type) {
	case nil, string, bool, float64, float32, int, int64, int32, uint, uint64, json.Number, ArgReference, ActorReference, *ArgReference, *ActorReference:
		return true
	case []any:
		for _, item := range n {
			if !organizationPlainValues(item) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, item := range n {
			if !organizationPlainValues(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (e *MemQLEngine) organizationPlainFilter(expr ExpressionNode, seen map[string]bool) bool {
	switch n := expr.(type) {
	case *ComparisonExpression:
		return organizationPlainValues(n.Value)
	case *LogicalExpression:
		return e.organizationPlainFilter(n.Left, seen) && e.organizationPlainFilter(n.Right, seen)
	case *NotExpression:
		return e.organizationPlainFilter(n.Target, seen)
	case *constantBoolExpression, *AccountScopeExpression, *RankScopeExpression, *ReadFloorExpression:
		return true
	case *SpecReferenceExpression:
		if seen[n.Name] || e.specs == nil {
			return false
		}
		spec, ok := e.specs.Lookup(n.Name)
		if !ok || spec.UsesAI {
			return false
		}
		seen[n.Name] = true
		defer delete(seen, n.Name)
		return e.organizationPlainFilter(spec.Expr, seen)
	case *ArrayPredicateExpression:
		return (n.Pred == nil || e.organizationPlainFilter(n.Pred, seen)) && organizationPlainValues(n.CountValue)
	default:
		return false
	}
}

func organizationRootShape(shape shapeTemplate) bool {
	switch n := shape.(type) {
	case *shapeObject:
		for _, field := range n.Fields {
			if !organizationRootShape(field) {
				return false
			}
		}
		return true
	case *shapeArray:
		for _, item := range n.Items {
			if !organizationRootShape(item) {
				return false
			}
		}
		return true
	case *shapeNodeFunc, *shapeLiteral:
		return true
	case *shapeJSONFunc:
		return organizationRootShape(n.Inner)
	default:
		return false
	}
}
