package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// providerFederationSet WRITES ITS ROWS FOR BOTH ROLES IT ADMITS, against a
// real engine and a real Postgres.
//
// The builtin is owner-or-developer (providersConfigAuthorized), and what it
// writes are v1:platform:globalVariable rows, which are a cluster owner's or
// stamped server code's to write (platform_variable_write_guard.go). So a
// developer's save lands only because the builtin, having authorized the
// developer, stamps internal origin for each row it composed. Without the stamp
// the Settings section would offer a developer a save the engine turns down.
//
// A VENDOR OF THIS TEST'S OWN. The real vendors' ids are read at provider load,
// and a partial set refuses boot; rows carrying their names must never land in
// the database the suites share. The two tables the builtin reads are extended
// for the length of this test, which is sequential, so nothing reads them
// meanwhile.
func TestProviderFederationSetWritesItsRowsForEveryRoleItAdmits(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	suffix := strings.ToUpper(strings.ReplaceAll(uniqueSuffix("federation-set"), "-", "_"))
	const vendor = "federationsettest"
	ruleName, accountName := "FEDERATION_SET_TEST_RULE_"+suffix, "FEDERATION_SET_TEST_ACCOUNT_"+suffix
	federationFieldNames[vendor] = map[string]string{"ruleId": ruleName, "accountId": accountName}
	federationRequiredFields[vendor] = []string{ruleName, accountName}
	t.Cleanup(func() {
		delete(federationFieldNames, vendor)
		delete(federationRequiredFields, vendor)
	})

	as := func(role auth.Role) context.Context {
		return rankActorCtx("federation-set-"+string(role)+"-"+suffix, role)
	}

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			rule, account := "rule-saved-by-"+string(role), "account-saved-by-"+string(role)
			_, err := eng.evaluateProviderFederationSetExpression(as(role), map[string]any{
				"vendor": vendor, "ruleId": rule, "accountId": account,
			})
			require.NoError(t, err, "a %s's federation ids were not saved", role)
			for name, want := range map[string]string{ruleName: rule, accountName: account} {
				got, err := eng.ResolveSystemVariable(context.Background(), name)
				require.NoError(t, err, "the id a %s saved under %s does not resolve", role, name)
				require.Equal(t, want, got)
			}
		})
	}

	// The roles the builtin does not admit reach no write at all.
	before, err := eng.ResolveSystemVariable(context.Background(), ruleName)
	require.NoError(t, err)
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleWriter, auth.RoleReader} {
		_, err := eng.evaluateProviderFederationSetExpression(as(role), map[string]any{
			"vendor": vendor, "ruleId": "rule-by-" + string(role), "accountId": "account-by-" + string(role),
		})
		require.Error(t, err, "a %s set the federation ids", role)
		require.Contains(t, err.Error(), "owner-or-developer")
	}
	after, err := eng.ResolveSystemVariable(context.Background(), ruleName)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused role's call changed a stored id")

	// The stamp is the builtin's, for the rows it composed. The developer's own
	// write of the same row is still refused.
	_, err = runSiteMutation(t, as(auth.RoleDeveloper), eng, "setGlobalVariable", map[string]any{
		"id": "var-" + strings.ToLower(strings.ReplaceAll(ruleName, "_", "-")), "name": ruleName, "value": "written directly",
	})
	require.Error(t, err, "a developer wrote a federation id with their own setGlobalVariable")
	require.Contains(t, err.Error(), variableWriteRefusal)
}
