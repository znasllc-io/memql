package memql

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/component"
)

func TestBuiltinDiagnosticRedactionDoesNotImplySecretCapability(t *testing.T) {
	for _, tc := range []struct {
		declaration string
		name        string
		secret      bool
	}{
		{`prompt string @enum("safe")`, "prompt", false},
		{`sectionKeys string @enum("safe")`, "sectionKeys", false},
		{`settings string @secret @enum("safe")`, "settings", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decl, err := languageParser.ParseBuiltinDecl(`@executor("help") builtin privateProbe { ` + tc.declaration + ` }`)
			require.NoError(t, err)
			fn, err := builtinDeclToFunction(decl, "privacy-test")
			require.NoError(t, err)
			// Registry lookups clone fields; both privacy meanings must survive.
			field := fn.BuiltinArgs.clone().Fields[0]
			require.Equal(t, tc.secret, workCapabilitySecret(field))
			args := map[string]any{tc.name: secretValue}
			validator := &functionValidator{}
			err = validator.validateFunctionArgs(&Function{Name: "privateProbe", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{field}}}, args)
			mustNotLeak(t, err)
			names := map[string]struct{}{}
			collectSecretArgNames(field, names)
			raw, err := json.Marshal(redactArgsForLog(args, names))
			require.NoError(t, err)
			require.NotContains(t, string(raw), secretValue)
			require.NotContains(t, redactSecretArgValues("invalid "+secretValue, names, args), secretValue)
			// Format errors follow a separate validation path from enums.
			field.Enum, field.Format = nil, "date-time"
			err = validator.validateFunctionArgs(&Function{Name: "privateProbe", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{field}}}, args)
			mustNotLeak(t, err)
		})
	}
}

func TestShopifyOverlayCredentialInputsRemainPrivateCapabilities(t *testing.T) {
	const origin = "../../dsl/shopify/overlay/builtins.memql"
	source, err := os.ReadFile(origin)
	require.NoError(t, err)
	want := map[string]string{"shopifyStoreAppSave": "clientSecret", "shopifyStorefrontTokenSet": "token"}
	for _, slice := range ExtractKeywordSlices(string(source), "builtin") {
		fieldName, ok := want[slice.Name]
		if !ok {
			continue
		}
		decl, err := languageParser.ParseBuiltinDecl(slice.Source)
		require.NoError(t, err)
		fn, err := builtinDeclToFunction(decl, origin)
		require.NoError(t, err)
		found := false
		for _, field := range fn.BuiltinArgs.Fields {
			if field.Name == fieldName {
				found = true
				require.True(t, field.Secret, slice.Name)
			}
		}
		require.True(t, found, slice.Name)
		e := &MemQLEngine{functions: newFunctionRegistry()}
		require.NoError(t, e.functions.add(fn))
		require.False(t, e.workCapabilityAllowed(askTestActor(), fn))
		_, err = e.workExecuteBuiltin(askTestActor(), map[string]any{"name": fn.Name, "arguments": map[string]any{}}, 0)
		require.ErrorContains(t, err, "capability is unavailable for this person")
		delete(want, slice.Name)
	}
	require.Empty(t, want, "both shipped overlay credential operations must be tested")
}

func TestBuiltinToolValidationRedactsDeclaredAndDiagnosticInputs(t *testing.T) {
	decl, err := languageParser.ParseBuiltinDecl(`@executor("help") builtin privateToolProbe {
  prompt string
  sectionKeys []string
  settings object @secret
  count int
}`)
	require.NoError(t, err)
	fn, err := builtinDeclToFunction(decl, "privacy-test")
	require.NoError(t, err)
	functions := newFunctionRegistry()
	require.NoError(t, functions.add(fn))
	var log bytes.Buffer
	e := &MemQLEngine{Component: &component.Component{Logger: slog.New(slog.NewTextHandler(&log, nil))}, functions: functions}
	tool := &Tool{Name: "privateToolProbe", Handler: &ToolHandler{Type: "function", FunctionName: fn.Name}, InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"sectionKeys":{"type":"array","items":{"type":"string"}},"settings":{"type":"object"},"count":{"type":"integer","minimum":100}}}`)}
	toolSchemaCache.Delete(tool.Name)
	t.Cleanup(func() { toolSchemaCache.Delete(tool.Name) })
	args := map[string]any{"prompt": secretValue, "sectionKeys": []any{secretValue}, "settings": map[string]any{"address": secretValue}, "count": float64(42)}
	err = e.validateToolArgs(tool, args)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secretValue)
	require.NotContains(t, log.String(), secretValue)
	require.Contains(t, log.String(), redactedArgValue)
	require.Contains(t, log.String(), "count", "ordinary validation context remains useful")
	// A failing value nested inside declared private settings must also be
	// removed from the model-facing JSON-schema error, not only the log map.
	tool.InputSchema = json.RawMessage(`{"type":"object","properties":{"settings":{"type":"object","properties":{"address":{"type":"integer","minimum":100}}}}}`)
	toolSchemaCache.Delete(tool.Name)
	err = e.validateToolArgs(tool, map[string]any{"settings": map[string]any{"address": float64(42)}})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "42")
	require.Contains(t, err.Error(), redactedArgValue)
}
