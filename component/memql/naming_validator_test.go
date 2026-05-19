package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestValidateNamingPrefix_StrictMode locks Decision 3: strict mode
// is default ON. A query / mutation / spec function whose name
// lacks the kind prefix is rejected at load time with a clear
// migration message.
func TestValidateNamingPrefix_StrictMode(t *testing.T) {
	cases := []struct {
		name      string
		fn        *languageParser.FunctionDef
		wantRule  string
		wantHint  string
		wantError bool
	}{
		{
			name: "query-missing-prefix",
			fn: &languageParser.FunctionDef{
				Name: "getUserById",
				Type: languageParser.FunctionTypeQuery,
			},
			wantRule:  "naming.query-prefix",
			wantHint:  "queryGetUserById",
			wantError: true,
		},
		{
			name: "mutation-missing-prefix",
			fn: &languageParser.FunctionDef{
				Name: "archiveUser",
				Type: languageParser.FunctionTypeMutation,
			},
			wantRule:  "naming.mutation-prefix",
			wantHint:  "mutationArchiveUser",
			wantError: true,
		},
		{
			name: "spec-missing-prefix",
			fn: &languageParser.FunctionDef{
				Name: "isActiveUser",
				Type: languageParser.FunctionTypeSpec,
			},
			wantRule:  "naming.spec-prefix",
			wantHint:  "specIsActiveUser",
			wantError: true,
		},
		{
			name: "query-with-correct-prefix",
			fn: &languageParser.FunctionDef{
				Name: "queryUserById",
				Type: languageParser.FunctionTypeQuery,
			},
			wantError: false,
		},
		{
			name: "mutation-with-correct-prefix",
			fn: &languageParser.FunctionDef{
				Name: "mutationArchiveUser",
				Type: languageParser.FunctionTypeMutation,
			},
			wantError: false,
		},
		{
			name: "spec-with-correct-prefix",
			fn: &languageParser.FunctionDef{
				Name: "specIsActive",
				Type: languageParser.FunctionTypeSpec,
			},
			wantError: false,
		},
		{
			name: "automation-not-prefix-gated",
			fn: &languageParser.FunctionDef{
				Name: "bootstrapCluster",
				Type: languageParser.FunctionTypeAutomation,
			},
			// Automations explicitly use verb-first names per
			// memql-naming-conventions.md; the validator must not
			// touch them.
			wantError: false,
		},
		{
			name: "logic-not-prefix-gated",
			fn: &languageParser.FunctionDef{
				Name: "logicBootstrapCluster",
				Type: languageParser.FunctionTypeLogic,
			},
			wantError: false,
		},
	}

	// Strict mode is the default. We don't set MEMQL_NAMING_LENIENT
	// so namingLenient() returns false (cached for the test process).
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNamingPrefix(tc.fn)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantRule != "" && !strings.Contains(err.Error(), tc.wantRule) {
					t.Errorf("error should mention rule %q, got %v", tc.wantRule, err)
				}
				if tc.wantHint != "" && !strings.Contains(err.Error(), tc.wantHint) {
					t.Errorf("error should suggest rename %q, got %v", tc.wantHint, err)
				}
				if !strings.Contains(err.Error(), "MEMQL_NAMING_LENIENT") {
					t.Errorf("error should advertise the escape valve, got %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("expected nil error, got %v", err)
				}
			}
		})
	}
}

// TestValidateNamingPrefix_NilOrEmpty exercises the no-op guards.
func TestValidateNamingPrefix_NilOrEmpty(t *testing.T) {
	if err := validateNamingPrefix(nil); err != nil {
		t.Errorf("nil funcDef should pass, got %v", err)
	}
	if err := validateNamingPrefix(&languageParser.FunctionDef{Type: languageParser.FunctionTypeQuery}); err != nil {
		t.Errorf("empty name should pass (caller handles), got %v", err)
	}
}
