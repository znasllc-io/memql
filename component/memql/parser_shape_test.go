package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseShapeTemplateNestedObject(t *testing.T) {
	query := `shape(withDepth(parentOf(concept==v1:message;id=="msg-7"),2),{"conversation":node("payload.title","id"),"messages":children({"id":node("id"),"author":createdBy({"id":node("id"),"name":node("payload.name")})})})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	require.Len(t, root.Fields, 2)

	conversation := assertShapeNodeTemplate(t, root.Fields["conversation"])
	require.Len(t, conversation.Fields, 2)
	require.Equal(t, "payload.title", conversation.Fields[0].Raw)
	require.Equal(t, "id", conversation.Fields[1].Raw)

	messages := assertShapeRelation(t, root.Fields["messages"], "children")
	messageObj := assertShapeObject(t, messages.Template)
	require.Contains(t, messageObj.Fields, "id")
	assertShapeNodeTemplate(t, messageObj.Fields["id"])

	require.Contains(t, messageObj.Fields, "author")
	author := assertShapeRelation(t, messageObj.Fields["author"], "createdby")
	assertShapeObject(t, author.Template)
}

func TestParseShapeTemplateArray(t *testing.T) {
	query := `shape(concept==v1:conversation,[{"id":node("id"),"children":children({"id":node("id")})},node("payload.title"),"static"])`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeArray(t, plan.ShapeTemplate)
	require.Len(t, root.Items, 3)

	firstObj := assertShapeObject(t, root.Items[0])
	require.Contains(t, firstObj.Fields, "id")
	assertShapeNodeTemplate(t, firstObj.Fields["id"])

	require.Contains(t, firstObj.Fields, "children")
	assertShapeRelation(t, firstObj.Fields["children"], "children")

	second := assertShapeNodeTemplate(t, root.Items[1])
	require.Len(t, second.Fields, 1)
	require.Equal(t, "payload.title", second.Fields[0].Raw)

	third := root.Items[2]
	literal, ok := third.(*shapeLiteral)
	require.True(t, ok)
	require.Equal(t, "static", literal.Value)
}

func TestParseShapeAICacheTTL(t *testing.T) {
	query := `shape(concept==v1:document,{"summary":si("docSummary", {"title":node("payload.title")}, "openai", 180)})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	summary, ok := root.Fields["summary"].(*shapeSIValue)
	require.True(t, ok)
	require.NotNil(t, summary.Invocation)
	require.NotNil(t, summary.Invocation.ProviderOverride)
	require.Equal(t, "openai", strings.TrimSpace(*summary.Invocation.ProviderOverride))
	require.NotNil(t, summary.Invocation.CacheSeconds)
	require.Equal(t, 180, *summary.Invocation.CacheSeconds)
	require.NotNil(t, summary.Data)
}

func assertShapeObject(t *testing.T, tmpl shapeTemplate) *shapeObject {
	t.Helper()
	obj, ok := tmpl.(*shapeObject)
	require.Truef(t, ok, "expected shapeObject, got %T", tmpl)
	return obj
}

func assertShapeArray(t *testing.T, tmpl shapeTemplate) *shapeArray {
	t.Helper()
	arr, ok := tmpl.(*shapeArray)
	require.Truef(t, ok, "expected shapeArray, got %T", tmpl)
	return arr
}

func assertShapeRelation(t *testing.T, tmpl shapeTemplate, name string) *shapeRelationFunc {
	t.Helper()
	rel, ok := tmpl.(*shapeRelationFunc)
	require.Truef(t, ok, "expected shapeRelationFunc, got %T", tmpl)
	require.Equal(t, strings.ToLower(name), strings.ToLower(rel.Relation))
	return rel
}

func assertShapeNodeTemplate(t *testing.T, tmpl shapeTemplate) *shapeNodeFunc {
	t.Helper()
	node, ok := tmpl.(*shapeNodeFunc)
	require.Truef(t, ok, "expected shapeNodeFunc, got %T", tmpl)
	return node
}

func TestParseShapeMatchWithSpec(t *testing.T) {
	query := `shape(concept==v1:customer,{"origin":match(case(hispanicName,"hisp"),case(asianName,"asian"),default("unknown"))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	require.Contains(t, root.Fields, "origin")

	match, ok := root.Fields["origin"].(*shapeMatchExpr)
	require.True(t, ok, "expected shapeMatchExpr, got %T", root.Fields["origin"])

	require.Len(t, match.Cases, 2)

	// First case: hispanicName
	specCond1, ok := match.Cases[0].Condition.(*shapeSpecCondition)
	require.True(t, ok, "expected shapeSpecCondition")
	require.Equal(t, "hispanicName", specCond1.SpecName)

	lit1, ok := match.Cases[0].Value.(*shapeLiteral)
	require.True(t, ok, "expected shapeLiteral")
	require.Equal(t, "hisp", lit1.Value)

	// Second case: asianName
	specCond2, ok := match.Cases[1].Condition.(*shapeSpecCondition)
	require.True(t, ok, "expected shapeSpecCondition")
	require.Equal(t, "asianName", specCond2.SpecName)

	lit2, ok := match.Cases[1].Value.(*shapeLiteral)
	require.True(t, ok, "expected shapeLiteral")
	require.Equal(t, "asian", lit2.Value)

	// Default
	require.NotNil(t, match.Default)
	defLit, ok := match.Default.(*shapeLiteral)
	require.True(t, ok, "expected shapeLiteral")
	require.Equal(t, "unknown", defLit.Value)
}

func TestParseShapeMatchWithComparison(t *testing.T) {
	query := `shape(concept==v1:customer,{"status":match(case(node("payload.status")=="active","Active"),case(node("payload.status")=="pending","Pending"),default("Unknown"))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	require.Contains(t, root.Fields, "status")

	match, ok := root.Fields["status"].(*shapeMatchExpr)
	require.True(t, ok, "expected shapeMatchExpr")

	require.Len(t, match.Cases, 2)

	// First case: node("payload.status") == "active"
	cmpCond1, ok := match.Cases[0].Condition.(*shapeComparisonCondition)
	require.True(t, ok, "expected shapeComparisonCondition")
	require.Equal(t, OpEq, cmpCond1.Operator)
	require.Equal(t, "active", cmpCond1.Right)

	nodeFunc1, ok := cmpCond1.Left.(*shapeNodeFunc)
	require.True(t, ok, "expected shapeNodeFunc")
	require.Len(t, nodeFunc1.Fields, 1)
	require.Equal(t, "payload.status", nodeFunc1.Fields[0].Raw)

	// Check value
	lit1, ok := match.Cases[0].Value.(*shapeLiteral)
	require.True(t, ok)
	require.Equal(t, "Active", lit1.Value)
}

func TestParseShapeMatchWithAIDefault(t *testing.T) {
	query := `shape(concept==v1:customer,{"origin":match(case(hispanicName,"hisp"),default(si("nameClassifier.v1",{"name":node("payload.middleName")})))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	match, ok := root.Fields["origin"].(*shapeMatchExpr)
	require.True(t, ok)

	require.Len(t, match.Cases, 1)
	require.NotNil(t, match.Default)

	aiValue, ok := match.Default.(*shapeSIValue)
	require.True(t, ok, "expected shapeSIValue")
	require.Equal(t, "nameClassifier.v1", aiValue.Invocation.TemplateId)
	require.NotNil(t, aiValue.Data)
}

func TestParseShapeMatchWithInOperator(t *testing.T) {
	query := `shape(concept==v1:customer,{"region":match(case(node("payload.country") in ("US","CA","MX"),"North America"),default("Other"))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	match, ok := root.Fields["region"].(*shapeMatchExpr)
	require.True(t, ok)

	require.Len(t, match.Cases, 1)

	cmpCond, ok := match.Cases[0].Condition.(*shapeComparisonCondition)
	require.True(t, ok)
	require.Equal(t, OpIn, cmpCond.Operator)

	rightList, ok := cmpCond.Right.([]any)
	require.True(t, ok)
	require.Len(t, rightList, 3)
	require.Equal(t, "US", rightList[0])
	require.Equal(t, "CA", rightList[1])
	require.Equal(t, "MX", rightList[2])
}

func TestParseShapeMatchDefaultOnly(t *testing.T) {
	query := `shape(concept==v1:customer,{"value":match(default("fallback"))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	match, ok := root.Fields["value"].(*shapeMatchExpr)
	require.True(t, ok)

	require.Len(t, match.Cases, 0)
	require.NotNil(t, match.Default)

	defLit, ok := match.Default.(*shapeLiteral)
	require.True(t, ok)
	require.Equal(t, "fallback", defLit.Value)
}

func TestParseShapeMatchMultipleCases(t *testing.T) {
	query := `shape(concept==v1:order,{"priority":match(case(node("payload.total")>10000,"critical"),case(node("payload.total")>5000,"high"),case(node("payload.total")>1000,"medium"),default("low"))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	match, ok := root.Fields["priority"].(*shapeMatchExpr)
	require.True(t, ok)

	require.Len(t, match.Cases, 3)

	// Check operators
	for i, expectedOp := range []ComparisonOperator{OpGt, OpGt, OpGt} {
		cmpCond, ok := match.Cases[i].Condition.(*shapeComparisonCondition)
		require.True(t, ok)
		require.Equal(t, expectedOp, cmpCond.Operator)
	}

	require.NotNil(t, match.Default)
}

func TestParseShapeJSONWithNode(t *testing.T) {
	query := `shape(concept==v1:customer,{"profileJSON":json(node("payload"))})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	jsonFunc, ok := root.Fields["profileJSON"].(*shapeJSONFunc)
	require.True(t, ok)
	require.NotNil(t, jsonFunc.Inner)

	nodeFunc, ok := jsonFunc.Inner.(*shapeNodeFunc)
	require.True(t, ok)
	require.Len(t, nodeFunc.Fields, 1)
	require.Equal(t, "payload", nodeFunc.Fields[0].Raw)
}

func TestParseShapeJSONWithObject(t *testing.T) {
	query := `shape(concept==v1:user,{"data":json({"id":node("id"),"name":node("payload.name")})})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	jsonFunc, ok := root.Fields["data"].(*shapeJSONFunc)
	require.True(t, ok)
	require.NotNil(t, jsonFunc.Inner)

	innerObj, ok := jsonFunc.Inner.(*shapeObject)
	require.True(t, ok)
	require.Contains(t, innerObj.Fields, "id")
	require.Contains(t, innerObj.Fields, "name")
}

func TestParseShapeJSONInAIData(t *testing.T) {
	query := `shape(concept==v1:incident,{"triage":si("incidentTriage.v1",{"incidentJSON":json(node("payload"))})})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	root := assertShapeObject(t, plan.ShapeTemplate)
	aiVal, ok := root.Fields["triage"].(*shapeSIValue)
	require.True(t, ok)
	require.NotNil(t, aiVal.Data)

	dataObj, ok := aiVal.Data.(*shapeObject)
	require.True(t, ok)

	jsonFunc, ok := dataObj.Fields["incidentJSON"].(*shapeJSONFunc)
	require.True(t, ok)
	require.NotNil(t, jsonFunc.Inner)
}

func TestParseShapeJSONRequiresArgument(t *testing.T) {
	query := `shape(concept==v1:customer,{"data":json()})`
	_, err := (&MemQLEngine{initialized: true}).Parse(query)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "json() requires an argument")
}
