package memql

// create_auth_session_source_db_test.go -- the LIVE half of memql#4592.
//
// Every RFC 8628 device-grant redemption 500'd in the field ("issue failed",
// 2026-08-25): the grant spends the device code, then persists the session
// with Source: "device_code" (component/identity/http/token_device.go), and
// the createAuthSession DSL mutation refused the value -- its `source` enum
// (and the authSession concept field behind it) declared only "bff_exchange"
// and "oidc_cookie". The device-grant HTTP suite runs the real handlers and
// the real store against a fake engine that accepts any mutation string, so
// nothing failed until the write met a real engine. This is the test that
// meets one.
//
// test/dslconformance/identity_session_source_enum_contract_test.go proves,
// statically, that every session Source literal the identity service emits is
// a value both DSL enums declare. This is the other direction, against the
// real engine and database: every value createAuthSession DECLARES is one the
// executor actually accepts and the row store actually keeps, and a value it
// does not declare is still refused with the enum error. Reading the list off
// the loaded function keeps the pair self-maintaining -- a new grant's value
// makes the static gate pass and this one exercise it, with no third list.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

func TestCreateAuthSession_EveryDeclaredSourceIsWritable(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	fn, _ := eng.functions.Get("createAuthSession")
	require.NotNil(t, fn, "createAuthSession must be registered from the embedded tree")
	require.NotNil(t, fn.ArgsSchema)

	var sources []string
	for _, field := range fn.ArgsSchema.Fields {
		if field != nil && field.Name == "source" {
			for _, v := range field.Enum {
				sources = append(sources, fmt.Sprint(v))
			}
		}
	}
	// The identity service's session funnel (session_row.go) is fed by three
	// writers today: the /oauth/token grants ("bff_exchange", "device_code",
	// token_session.go) and the browser cookie path ("oidc_cookie",
	// complete.go). A declared list missing any of them means a live grant's
	// session persist is refused AFTER its single-use credential is spent --
	// the exact field failure this test pins.
	for _, emitted := range []string{"bff_exchange", "oidc_cookie", "device_code"} {
		require.Contains(t, sources, emitted,
			"createAuthSession.source does not declare %q, so that grant's session write is refused by the real engine (enum: %v)",
			emitted, sources)
	}

	for _, source := range sources {
		t.Run(source, func(t *testing.T) {
			rowID := runMutation(t, ctx, eng, "createAuthSession", map[string]any{
				"sessionId": "authsession-4592-" + strings.ReplaceAll(source, "_", "-") + "-" + id.NewShortId(),
				"subject":   "v1:identity:user:4592-probe",
				"tokenHash": strings.Repeat("a", 64),
				"source":    source,
				"expiresAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
			})
			require.Contains(t, rowID, "authSession", "the write must land on v1:identity:authSession, got %s", rowID)
		})
	}

	// The gate is still armed: a value the enum does not name is refused
	// before any row is written.
	t.Run("undeclared value is refused", func(t *testing.T) {
		// languageParser.QuoteString, not %q: Go's escape grammar is not the
		// MemQL lexer's (TestDSLCallStringsDoNotUseGoQuoting).
		call := fmt.Sprintf(`mutation createAuthSession(sessionId: %s, subject: "v1:identity:user:4592-probe", tokenHash: %s, source: "notASourceAnyoneEmits", expiresAt: %s)`,
			languageParser.QuoteString("authsession-4592-refused-"+id.NewShortId()),
			languageParser.QuoteString(strings.Repeat("b", 64)),
			languageParser.QuoteString(time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)))
		_, err := eng.Execute(ctx, call)
		require.Error(t, err)
		require.Contains(t, err.Error(), "is not in enum")
	})
}
