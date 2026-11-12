package dslconformance

import "testing"

// An account's export and a move of its rows are audited under target types of
// their own (account export, design record 2026-11-12, section H). Their writers
// live in integrations/accounts, outside the trees the writer walk above reads,
// so the two values are pinned here on both enums: either one refusing them
// would drop the durable row and leave only a log line.
func TestAuditEventAcceptsTheAccountTargetTypes(t *testing.T) {
	mutation, concept := auditEventEnumsFromDSL(t)
	for _, want := range []string{"accountExport", "account"} {
		if !mutation["targetType"][want] {
			t.Errorf("createAuditEvent.targetType does not accept %q (got %v)", want, mutation["targetType"].sorted())
		}
		if !concept["targetType"][want] {
			t.Errorf("concept auditEvent.targetType does not accept %q (got %v)", want, concept["targetType"].sorted())
		}
	}
}
