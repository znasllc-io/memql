package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// Phase 3 smoke tests for the policy evaluator. Build a registry by
// hand (bypassing the embedded loader) and dispatch through
// engine.EvaluatePolicy to verify:
//   - tier mismatch errors when opts.RequiredTier doesn't match
//   - unknown policy errors cleanly
//   - bool body resolves true/false from ctx
//   - caller.* reads pull from the auth.AccessContext
//   - cycle detection fires when a policy is re-entered

func TestEvaluatePolicy_BoolBody_FromCtx(t *testing.T) {
	registry := mustBuildPolicyRegistry(t, `@tier("bff")
@description("ctx field test")
func (Policy) ctxField(ctx any) bool {
  return ctx.vendor == "anam"
}`)
	engine := &MemQLEngine{policyFunctions: registry}

	result, trace, err := engine.EvaluatePolicy(context.Background(), "ctxField", map[string]any{"vendor": "anam"}, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, true, result)
	require.NotNil(t, trace)
	require.Equal(t, "ctxField", trace.Name)
	require.Equal(t, "bff", trace.Tier)

	result, _, err = engine.EvaluatePolicy(context.Background(), "ctxField", map[string]any{"vendor": "simli"}, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, false, result)
}

func TestEvaluatePolicy_CtxInputPathExplicitAndShorthand(t *testing.T) {
	// Verifies the ctx-envelope rule: caller args land under
	// ctx.input. The body should be able to read them via the
	// canonical `ctx.input.X` path AND the shorthand `ctx.X` path
	// (which the resolver auto-falls-back to ctx.input.X).
	registryExplicit := mustBuildPolicyRegistry(t, `@tier("bff")
@description("explicit ctx.input.<key> read")
func (Policy) ctxInputExplicit(ctx any) bool {
  return ctx.input.vendor == "anam"
}`)
	engine := &MemQLEngine{policyFunctions: registryExplicit}
	result, _, err := engine.EvaluatePolicy(context.Background(), "ctxInputExplicit", map[string]any{"vendor": "anam"}, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, true, result)

	registryShort := mustBuildPolicyRegistry(t, `@tier("bff")
@description("shorthand ctx.<key> read")
func (Policy) ctxShorthand(ctx any) bool {
  return ctx.vendor == "anam"
}`)
	engine2 := &MemQLEngine{policyFunctions: registryShort}
	result2, _, err := engine2.EvaluatePolicy(context.Background(), "ctxShorthand", map[string]any{"vendor": "anam"}, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, true, result2)
}

func TestEvaluatePolicy_TierMismatch(t *testing.T) {
	registry := mustBuildPolicyRegistry(t, `@tier("bff")
@description("bff-only policy")
func (Policy) bffOnly(_ any) bool {
  return true
}`)
	engine := &MemQLEngine{policyFunctions: registry}

	_, _, err := engine.EvaluatePolicy(context.Background(), "bffOnly", nil, PolicyEvalOptions{RequiredTier: "core"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "tier")
}

func TestEvaluatePolicy_UnknownPolicy(t *testing.T) {
	engine := &MemQLEngine{policyFunctions: newPolicyFunctionRegistry()}
	_, _, err := engine.EvaluatePolicy(context.Background(), "doesNotExist", nil, PolicyEvalOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered")
}

func TestEvaluatePolicy_CallerReference(t *testing.T) {
	registry := mustBuildPolicyRegistry(t, `@tier("core")
@audited
@description("admin check")
func (Policy) requiresAdmin(_ any) bool {
  return caller.role == "admin"
}`)
	engine := &MemQLEngine{policyFunctions: registry}

	access := &auth.AccessContext{UserId: "u-1", Role: auth.RoleAdmin}
	ctx := auth.ContextWithAccess(context.Background(), access)
	result, _, err := engine.EvaluatePolicy(ctx, "requiresAdmin", nil, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, true, result)

	access2 := &auth.AccessContext{UserId: "u-2", Role: auth.RoleReader}
	ctx2 := auth.ContextWithAccess(context.Background(), access2)
	result, _, err = engine.EvaluatePolicy(ctx2, "requiresAdmin", nil, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, false, result)
}

// TestPolicyLoad_RejectsSpecShapedBody — Phase C check. A policy
// whose body is a pure caller-only boolean (no policy()/spec()
// calls, no policy-only annotations, no ctx args) is structurally
// a context-spec and should be refused at registration. The error
// message must hint the migration path so the author knows to move
// the file to dsl/v1/specs/.
func TestPolicyLoad_RejectsSpecShapedBody(t *testing.T) {
	// Spec-shaped: caller-only boolean, no @audited / @frontend_visible
	// / @cacheable / @traces_persisted. Loader must reject.
	src := `@tier("core")
@description("admin check -- should have been a spec")
func (Policy) badSpecShapedPolicy(_ any) bool {
  return caller.role == "admin"
}`
	_, err := parsePolicyFunctionFile("badSpecShapedPolicy.memql", "core", []byte(src))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no policy-only annotations and no sub-policy calls")
	require.Contains(t, err.Error(), `spec("badSpecShapedPolicy")`)

	// Same body but with @audited -- the author's intent that this
	// be a policy is explicit; accept.
	srcAudited := `@tier("core")
@audited
@description("admin check -- explicit policy")
func (Policy) explicitAuditedPolicy(_ any) bool {
  return caller.role == "admin"
}`
	_, err = parsePolicyFunctionFile("explicitAuditedPolicy.memql", "core", []byte(srcAudited))
	require.NoError(t, err)

	// Composes a spec -- legitimate policy work, accept.
	srcComposes := `@tier("bff")
@description("admin gate")
func (Policy) gateOnSpec(_ any) bool {
  return spec("requiresAdmin")
}`
	_, err = parsePolicyFunctionFile("gateOnSpec.memql", "bff", []byte(srcComposes))
	require.NoError(t, err)

	// Reads ctx.input.X -- takes args, has to be a policy.
	srcCtxArgs := `@tier("bff")
@description("vendor check")
func (Policy) vendorPolicy(ctx any) bool {
  return ctx.vendor == "anam"
}`
	_, err = parsePolicyFunctionFile("vendorPolicy.memql", "bff", []byte(srcCtxArgs))
	require.NoError(t, err)
}

// TestEvaluateSpec_ContextSpec_AdminRole — exercises the Phase A
// spec broadening: a context-spec body reads `caller.role` and is
// callable from Go via engine.EvaluateSpec AND from a policy body
// via the new `spec("name")` builtin. Verifies the row-vs-context
// classifier picked SpecKindContext and that the auth context flows
// through.
func TestEvaluateSpec_ContextSpec_AdminRole(t *testing.T) {
	registry := newSpecRegistry()
	src := `@description("admin check")
@useShape(callerActor)
spec requiresAdmin {
  caller.role == "admin"
}`
	specObj, err := parseSpecMemQL("requiresAdmin.memql", []byte(src))
	require.NoError(t, err)
	require.Equal(t, SpecKindContext, specObj.Kind, "expected context-spec classification")
	require.NoError(t, registry.add(specObj))

	engine := &MemQLEngine{specs: registry}

	// Admin caller — spec should return true.
	adminAccess := &auth.AccessContext{UserId: "u-admin", Role: auth.RoleAdmin}
	ctx := auth.ContextWithAccess(context.Background(), adminAccess)
	ok, err := engine.EvaluateSpec(ctx, "requiresAdmin")
	require.NoError(t, err)
	require.True(t, ok)

	// Reader caller — spec should return false.
	readerAccess := &auth.AccessContext{UserId: "u-reader", Role: auth.RoleReader}
	ctx2 := auth.ContextWithAccess(context.Background(), readerAccess)
	ok, err = engine.EvaluateSpec(ctx2, "requiresAdmin")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestEvaluateSpec_RejectsRowSpec — calling EvaluateSpec on a
// row-spec (payload-only body) returns an error pointing the caller
// to the query-filter path. Row-specs are not procedurally callable.
func TestEvaluateSpec_RejectsRowSpec(t *testing.T) {
	registry := newSpecRegistry()
	src := `@description("row spec")
@useShape(statusRowTrait)
spec rowOnly {
  payload.status == "active"
}`
	specObj, err := parseSpecMemQL("rowOnly.memql", []byte(src))
	require.NoError(t, err)
	require.Equal(t, SpecKindRow, specObj.Kind)
	require.NoError(t, registry.add(specObj))

	engine := &MemQLEngine{specs: registry}
	_, err = engine.EvaluateSpec(context.Background(), "rowOnly")
	require.Error(t, err)
	require.Contains(t, err.Error(), "row-spec")
}

// TestPolicyBody_SpecBuiltin — policy body composes a context-spec
// via `spec("name")`. Validates the spec() builtin in
// evaluatePolicyFunctionCall and that the result threads through
// the policy's own return path.
func TestPolicyBody_SpecBuiltin(t *testing.T) {
	specRegistry := newSpecRegistry()
	specSrc := `@description("admin check")
@useShape(callerActor)
spec requiresAdmin {
  caller.role == "admin"
}`
	specObj, err := parseSpecMemQL("requiresAdmin.memql", []byte(specSrc))
	require.NoError(t, err)
	require.NoError(t, specRegistry.add(specObj))

	policyRegistry := mustBuildPolicyRegistry(t, `@tier("bff")
@description("admin gate composing a spec")
func (Policy) canViewAdmin(_ any) bool {
  return spec("requiresAdmin")
}`)

	engine := &MemQLEngine{specs: specRegistry, policyFunctions: policyRegistry}

	access := &auth.AccessContext{UserId: "u-admin", Role: auth.RoleAdmin}
	ctx := auth.ContextWithAccess(context.Background(), access)
	result, _, err := engine.EvaluatePolicy(ctx, "canViewAdmin", nil, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, true, result)

	access2 := &auth.AccessContext{UserId: "u-reader", Role: auth.RoleReader}
	ctx2 := auth.ContextWithAccess(context.Background(), access2)
	result, _, err = engine.EvaluatePolicy(ctx2, "canViewAdmin", nil, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, false, result)
}

func TestEvaluatePolicy_SubPolicyCall(t *testing.T) {
	// Compose two policies — child returns the answer, parent
	// dispatches via policy() and returns the same result. Verifies
	// the policy() builtin reaches engine.EvaluatePolicy and that
	// the parent trace captures the sub-call.
	childSrc := `@tier("bff")
@description("child")
func (Policy) childPolicy(ctx any) bool {
  return ctx.flag == "yes"
}`
	parentSrc := `@tier("bff")
@description("parent")
func (Policy) parentPolicy(_ any) bool {
  return policy("childPolicy", { flag: "yes" })
}`
	reg := newPolicyFunctionRegistry()
	child, err := parsePolicyFunctionFile("childPolicy.memql", "bff", []byte(childSrc))
	require.NoError(t, err)
	reg.byName[child.Name] = child
	parent, err := parsePolicyFunctionFile("parentPolicy.memql", "bff", []byte(parentSrc))
	require.NoError(t, err)
	reg.byName[parent.Name] = parent

	engine := &MemQLEngine{policyFunctions: reg}
	result, trace, err := engine.EvaluatePolicy(context.Background(), "parentPolicy", nil, PolicyEvalOptions{})
	require.NoError(t, err)
	require.Equal(t, true, result)
	require.NotNil(t, trace)
	require.Equal(t, "parentPolicy", trace.Name)
	require.Len(t, trace.Subcalls, 1, "expected one captured subcall")
	require.Equal(t, "childPolicy", trace.Subcalls[0].Name)
}

func TestEvaluatePolicy_CycleDetection(t *testing.T) {
	registry := mustBuildPolicyRegistry(t, `@tier("bff")
@description("cycle test")
func (Policy) loopy(_ any) bool {
  return true
}`)
	engine := &MemQLEngine{policyFunctions: registry}

	state := &policyEvalState{inFlight: map[string]struct{}{"loopy": {}}}
	ctx := withPolicyEvalState(context.Background(), state)
	_, _, err := engine.EvaluatePolicy(ctx, "loopy", nil, PolicyEvalOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle detected")
}

// mustBuildPolicyRegistry parses the given source as a policy file
// and wraps it in a one-policy registry for the test. The origin
// path is synthesised from the parsed function name so the
// filename-vs-function-name check in parsePolicyFunctionFile is
// satisfied without naming the test source file specially.
func mustBuildPolicyRegistry(t *testing.T, source string) *PolicyFunctionRegistry {
	t.Helper()
	name := extractFuncName(source)
	require.NotEmpty(t, name, "test source must define a Policy function")
	fn, err := parsePolicyFunctionFile(name+".memql", inferTierFromSource(source), []byte(source))
	require.NoError(t, err)
	reg := newPolicyFunctionRegistry()
	reg.byName[fn.Name] = fn
	return reg
}

func extractFuncName(source string) string {
	const marker = "func (Policy) "
	idx := indexOf(source, marker)
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := start
	for end < len(source) {
		c := source[end]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			end++
			continue
		}
		break
	}
	return source[start:end]
}

// inferTierFromSource is a tiny helper: parsePolicyFunctionFile
// validates that @tier matches dirTier, so the test fixture has to
// supply a tier consistent with the source's @tier annotation.
// Phase 3 tests don't exercise the directory check; they just need
// any non-empty dirTier that matches.
func inferTierFromSource(source string) string {
	switch {
	case bytesContain(source, `@tier("core")`):
		return "core"
	case bytesContain(source, `@tier("bff")`):
		return "bff"
	}
	return "bff"
}

func bytesContain(s, sub string) bool {
	return len(sub) > 0 && (len(s) >= len(sub)) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Phase 3 leaves filename-validation in parsePolicyFunctionFile
// strict. The test fixture file is named test-policy.memql, so
// rename for parse-cycle compatibility by giving the helper a
// stub. The real loader path runs against on-disk files.
//
// Also: parsePolicyFunctionFile enforces filename == function name.
// Tests above use names that don't match "test-policy"; route the
// helper through a name-aware path so the helper rewrites origin.
// (Kept inline to keep the suite self-contained.)
var _ = func() bool {
	// no-op: future Phase 6 expansion goes here.
	return true
}()
