package webauthn

// The BE/BS round-trip the passkey path never had (memql#3606).
//
// Registration stored no backupEligible flag at all -- not false, ABSENT.
// At login the row read the zero value, a synced authenticator asserted
// true, and go-webauthn refused the assertion:
//
//	webauthn: assertion verification failed: Backup Eligible flag
//	inconsistency detected during login validation
//
// Every synced passkey was affected, on every cluster. The cause was not
// in this package -- the store passed the flags correctly -- but in how
// the engine's parser scanned `backupEligible:true`, which bound a
// garbage argument and dropped the real one (see
// component/language/parser/colon_value_literal_test.go).
//
// The absence of THIS test is why that shipped. Both store writes assert
// what the caller cannot see for itself: that the query they emit is one
// the engine parses back into the flags that went in. They run the REAL
// parser over the REAL emitted string, so they fail against the broken
// engine and pass against the fixed one -- a unit test of the store alone
// would have passed throughout.

import (
	"context"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// parsedArgs runs the production parser over an emitted query and
// returns the argument map the engine would hand the mutation.
func parsedArgs(t *testing.T, query string) map[string]any {
	t.Helper()
	expr, err := langparser.ParseExpression(query)
	if err != nil {
		t.Fatalf("the engine cannot parse the query this store emits:\n  %s\n  %v", query, err)
	}
	call, ok := expr.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("query parsed as %T, want a mutation call:\n  %s", expr, query)
	}
	return call.Args
}

// requireArg fails with the emitted query in hand, because "the flag is
// missing" is only actionable next to the text that lost it.
func requireArg(t *testing.T, args map[string]any, name string, want any, query string) {
	t.Helper()
	got, ok := args[name]
	if !ok {
		t.Errorf("argument %q is ABSENT from the parsed args %#v.\n  emitted: %s\n"+
			"  An absent flag is not a false one: the mutation renders a payload with the "+
			"field missing, the row reads the zero value at login, and a synced "+
			"authenticator asserting the opposite is refused.", name, args, query)
		return
	}
	if got != want {
		t.Errorf("argument %q = %#v, want %#v\n  emitted: %s", name, got, want, query)
	}
}

func TestCreateRoundTripsBackupFlags(t *testing.T) {
	// BE=true / BS=true is the synced-passkey case -- an iCloud Keychain
	// or Google Password Manager credential, which is what most people
	// actually register, and the case that was broken.
	for _, tc := range []struct {
		name           string
		backupEligible bool
		backupState    bool
	}{
		{"synced credential", true, true},
		{"eligible but not yet backed up", true, false},
		{"device-bound credential", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &captureEngine{}
			store := &Store{Engine: engine}

			err := store.Create(context.Background(), "pk-1", "v1:identity:user:u1", "Work laptop",
				&RegisteredCredential{
					CredentialId:   "cred-1",
					PublicKey:      "pk-cose",
					SignCount:      0,
					AAGUID:         "fbfc3007-154e-4ecc-8c0b-6e020557d7bd",
					Transports:     []string{"hybrid", "internal"},
					BackupEligible: tc.backupEligible,
					BackupState:    tc.backupState,
				}, "v1:identity:user:u1")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			query, _ := engine.only(t)
			args := parsedArgs(t, query)
			requireArg(t, args, "backupEligible", tc.backupEligible, query)
			requireArg(t, args, "backupState", tc.backupState, query)
		})
	}
}

// TestRecordAssertionRoundTripsBackupFlags covers the write on the OTHER
// side of the same field. It re-passes backupEligible from the stored row
// (the flag is immutable by spec) while re-stamping backupState from the
// ceremony, so a drop here would silently rewrite an immutable flag to
// false on the user's next successful login -- breaking every login after
// the one that worked.
func TestRecordAssertionRoundTripsBackupFlags(t *testing.T) {
	engine := &captureEngine{}
	store := &Store{Engine: engine}

	row := &Row{
		ID:             "v1:identity:identity:pk-1",
		CredentialId:   "cred-1",
		PublicKey:      "pk-cose",
		AAGUID:         "fbfc3007-154e-4ecc-8c0b-6e020557d7bd",
		Transports:     []string{"hybrid", "internal"},
		BackupEligible: true,
		RegisteredBy:   "v1:identity:user:u1",
	}
	if err := store.RecordAssertion(context.Background(), row, 7, true, time.Now().UTC()); err != nil {
		t.Fatalf("RecordAssertion: %v", err)
	}

	query, _ := engine.only(t)
	args := parsedArgs(t, query)
	requireArg(t, args, "backupEligible", true, query)
	requireArg(t, args, "backupState", true, query)
}
