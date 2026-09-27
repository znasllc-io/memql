package memql

// sense_diagnose_load_test.go -- the gRPC Diagnose runs the engine's load over
// the document when its file_path places it in the tree, and returns Lower's
// refusals beside Diagnose's own, each with its rule code (memql#5434).

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// loadingRegistry is a Sense registry provider with an empty vocabulary that
// can run the load: it answers a fixed refusal for one path and records what
// it was asked.
type loadingRegistry struct {
	mu    sync.Mutex
	asked []string
}

func (r *loadingRegistry) FunctionNames() []string                        { return nil }
func (r *loadingRegistry) FunctionGet(string) (*sense.FunctionInfo, bool) { return nil, false }
func (r *loadingRegistry) ConceptNames() []string                         { return nil }
func (r *loadingRegistry) ConceptGet(string) (*sense.ConceptInfo, bool)   { return nil, false }
func (r *loadingRegistry) SpecNames() []string                            { return nil }
func (r *loadingRegistry) SpecGet(string) (*sense.SpecInfo, bool)         { return nil, false }
func (r *loadingRegistry) ToolNames() []string                            { return nil }
func (r *loadingRegistry) ToolGet(string) (*sense.ToolInfo, bool)         { return nil, false }
func (r *loadingRegistry) PromptNames() []string                          { return nil }
func (r *loadingRegistry) PromptGet(string) (*sense.PromptInfo, bool)     { return nil, false }
func (r *loadingRegistry) ProviderNames() []string                        { return nil }
func (r *loadingRegistry) ProviderGet(string) (*sense.ProviderInfo, bool) { return nil, false }
func (r *loadingRegistry) ShapeNames() []string                           { return nil }
func (r *loadingRegistry) ShapeGet(string) (*sense.ShapeInfo, bool)       { return nil, false }
func (r *loadingRegistry) IntegrationCapabilities() []string              { return nil }

func (r *loadingRegistry) LoadDiagnostics(_ string, filePath string) []sense.Diagnostic {
	r.mu.Lock()
	r.asked = append(r.asked, filePath)
	r.mu.Unlock()
	if filePath != "beta/queries.memql" {
		return nil
	}
	return []sense.Diagnostic{{
		Range:    sense.Range{Start: sense.Position{Line: 3, Column: 17}, End: sense.Position{Line: 3, Column: 26}},
		Severity: sense.SeverityError,
		Message:  "`row.shine` does not lower in a query filter: `shine` is not a declared field of v1:beta:widget",
		Code:     "lower_unknown_field",
	}}
}

func diagnoseOver(t *testing.T, reg *loadingRegistry, filePath string) []*memqlv1.SenseDiagnostic {
	t.Helper()
	s, cs := newAuthoringSession(t, auth.RoleWriter, "writer-1")
	s.service.sense = sense.New(reg)
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m-diag",
		Payload: &memqlv1.MemqlClientMessage_SenseDiagnose{SenseDiagnose: &memqlv1.SenseDiagnoseMsg{
			RequestId: "r-diag",
			Source:    "@unbounded(\"fixture\")\nquery widget shiningBetaWidgets {\n  filter row => row.shine == true\n}\n",
			FilePath:  filePath,
		}},
	}
	require.NoError(t, s.handleSenseDiagnose(env, env.GetSenseDiagnose()))
	result := awaitSent(t, cs).GetSenseDiagnoseResult()
	require.NotNil(t, result, "reply must be a SenseDiagnoseResult")
	return result.GetDiagnostics()
}

func TestSenseDiagnoseCarriesTheLoadsRefusalsWithTheirCodes(t *testing.T) {
	reg := &loadingRegistry{}
	diags := diagnoseOver(t, reg, "beta/queries.memql")
	var refusal *memqlv1.SenseDiagnostic
	for _, d := range diags {
		if d.GetCode() == "lower_unknown_field" {
			refusal = d
		}
	}
	require.NotNil(t, refusal, "the load's refusal must be on the reply: %v", diags)
	assert.Equal(t, memqlv1.SenseSeverity_SENSE_SEVERITY_ERROR, refusal.GetSeverity())
	assert.Equal(t, int32(3), refusal.GetRange().GetStart().GetLine())
	assert.Equal(t, int32(17), refusal.GetRange().GetStart().GetColumn())
	assert.Equal(t, int32(26), refusal.GetRange().GetEnd().GetColumn())

	// Without the path there is no load -- the engine is not even asked.
	reg2 := &loadingRegistry{}
	for _, d := range diagnoseOver(t, reg2, "") {
		assert.NotEqual(t, "lower_unknown_field", d.GetCode(), "no path, no load refusal")
	}
	assert.Empty(t, reg2.asked, "an untitled document must not reach the load")
}
