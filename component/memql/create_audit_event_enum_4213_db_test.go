package memql

// create_audit_event_enum_4213_db_test.go -- the LIVE half of memql#4213.
//
// test/dslconformance/identity_audit_enum_contract_test.go proves, statically,
// that every targetType the identity service's Go writers emit is a value the
// DSL enums declare. This is the other direction, against the real engine and
// database: every value createAuditEvent DECLARES is one the executor actually
// accepts and the row store actually keeps, and a value it does not declare is
// still refused with the exact error the issue reported
// (`argument "targetType": value X is not in enum [...]`).
//
// Reading the list off the loaded function rather than pinning it keeps the
// two tests self-maintaining: adding a writer value to the DSL makes the
// static gate pass and this one exercise it, with no third list to update.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

func TestCreateAuditEvent_EveryDeclaredTargetTypeIsWritable(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	fn, _ := eng.functions.Get("createAuditEvent")
	require.NotNil(t, fn, "createAuditEvent must be registered from the embedded tree")
	require.NotNil(t, fn.ArgsSchema)

	var targetTypes []string
	for _, field := range fn.ArgsSchema.Fields {
		if field != nil && field.Name == "targetType" {
			for _, v := range field.Enum {
				targetTypes = append(targetTypes, fmt.Sprint(v))
			}
		}
	}
	// The writers emit fifteen distinct values today (memql#4213 sweep). A
	// shorter list means the enum was narrowed or the parse lost it.
	require.GreaterOrEqual(t, len(targetTypes), 15, "createAuditEvent.targetType enum: %v", targetTypes)

	for _, tt := range targetTypes {
		t.Run(tt, func(t *testing.T) {
			rowID := runMutation(t, ctx, eng, "createAuditEvent", map[string]any{
				"eventId":    "audit-4213-" + strings.ToLower(tt) + "-" + id.NewShortId(),
				"occurredAt": time.Now().UTC().Format(time.RFC3339Nano),
				"category":   "auth",
				"action":     "enum_probe_" + strings.ToLower(tt),
				"targetType": tt,
				"targetId":   "probe-" + tt,
				"outcome":    "success",
			})
			require.Contains(t, rowID, "auditEvent", "the write must land on v1:identity:auditEvent, got %s", rowID)
		})
	}

	// The gate is still armed: a value the enum does not name is refused
	// before any row is written, with the error shape the issue quoted.
	t.Run("undeclared value is refused", func(t *testing.T) {
		// languageParser.QuoteString, not %q: Go's escape grammar is not the
		// MemQL lexer's (TestDSLCallStringsDoNotUseGoQuoting).
		call := fmt.Sprintf(`mutation createAuditEvent(eventId: %s, occurredAt: %s, category: "auth", action: "enum_probe_refused", targetType: "notAConceptAnyoneDeclared")`,
			languageParser.QuoteString("audit-4213-refused-"+id.NewShortId()),
			languageParser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)))
		_, err := eng.Execute(ctx, call)
		require.Error(t, err)
		require.Contains(t, err.Error(), "is not in enum")
	})
}
