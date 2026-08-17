package memql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// recovery_key_server_only_db_test.go -- the ENGINE half of the recovery-key
// @serverOnly contract, against the real loaded constructs.
//
// The whole break-glass surface is @serverOnly: two reads
// (activeRecoveryKeys, recoveryKeyByHash) and four writes
// (createRecoveryKeyIdentity, claimRecoveryKey, redeemRecoveryKey,
// deactivateRecoveryKey). That is correct and must stay -- a wire-reachable
// createRecoveryKeyIdentity would let any caller mint themselves an
// owner-equivalent credential, and a wire-reachable recoveryKeyByHash would
// turn the redeem lookup into an oracle.
//
// But strictness alone is not the property. An annotation nothing can satisfy
// is a feature that does not run, and that is exactly what shipped:
// component/identity/recoverykey issued all six constructs on the caller's
// unstamped context, so every one of them was refused. The boot invariant
// could not read, so no cluster minted an owner recovery key; `memql
// recovery-key claim` exited 1; and the redeem path could not resolve a
// presented key. Clusters booted with no break-glass route for their owner and
// said so only in a WARN.
//
// So BOTH halves are asserted here, per construct: a client-origin call is
// refused BY THE SERVER-ONLY GATE (not by something else that happens to
// fail), and an internal-origin call gets past it. Modelled on
// TestWorkerTokensForUserIsServerOnlyAndInternalOriginPasses, which pins the
// same pair for the same reason (memql#3063).
//
// Postgres-gated, like its neighbours in this package.
func TestRecoveryKeyConstructsAreServerOnlyAndInternalOriginPasses(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)

	// A row id this test owns, so the write cases act on nothing real. The
	// writes are what make the internal-origin half meaningful: asserting only
	// that the READS pass would leave the four mutations covered on the
	// refusal side alone, which is the half that was never broken.
	const probeId = "rk-serveronly-probe"

	cases := []struct {
		// construct is the @serverOnly name, and the string the refusal must
		// name -- so a test that starts passing for another reason is visible.
		construct string
		call      string
		// exposure says what a wire-reachable version of this construct would
		// hand a caller. Kept per-case rather than in the header because the
		// answer differs, and "it is server-only" is not a reason.
		exposure string
	}{
		{
			construct: "activeRecoveryKeys",
			call:      `query activeRecoveryKeys(userId:"user-rk-probe")`,
			exposure:  "enumeration of another owner's live break-glass credential rows behind a caller-supplied userId",
		},
		{
			construct: "recoveryKeyByHash",
			call:      `query recoveryKeyByHash(keyHash:"0000000000000000000000000000000000000000000000000000000000000000")`,
			exposure:  "an oracle: confirm a guessed key hash exists without redeeming it",
		},
		{
			construct: "createRecoveryKeyIdentity",
			call: fmt.Sprintf(`mutation createRecoveryKeyIdentity(identityId:%q,userId:"user-rk-probe",`+
				`label:"probe",keyHash:"probe-hash",boundOwnerUserId:"user-rk-probe",`+
				`mintedBy:"test",rotatedFrom:"")`, probeId),
			exposure: "self-minting an owner-equivalent credential -- the most severe of the six",
		},
		{
			construct: "claimRecoveryKey",
			call: fmt.Sprintf(`mutation claimRecoveryKey(identityId:%q,claimedAt:"2026-08-17T12:00:00Z",`+
				`claimedFromIP:"203.0.113.7")`, probeId),
			exposure: "stamping somebody else's key as claimed, which is the signal an operator holds it",
		},
		{
			construct: "redeemRecoveryKey",
			call: fmt.Sprintf(`mutation redeemRecoveryKey(identityId:%q,redeemedAt:"2026-08-17T12:00:00Z",`+
				`redeemedFromIP:"203.0.113.7")`, probeId),
			exposure: "spending another owner's key, denying them the route back in",
		},
		{
			construct: "deactivateRecoveryKey",
			call:      fmt.Sprintf(`mutation deactivateRecoveryKey(identityId:%q)`, probeId),
			exposure:  "retiring another owner's key -- a silent lockout with no audit of who did it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.construct, func(t *testing.T) {
			// readMergeTestEngine's context carries a token but NOT internal
			// origin, which is exactly the shape a wire call arrives in.
			t.Run("a client-origin call is refused", func(t *testing.T) {
				_, err := eng.Execute(ctx, tc.call)
				if err == nil {
					t.Fatalf("%s answered a client-origin call, which puts %s on the wire. "+
						"Either @serverOnly was dropped from the construct or the gate stopped "+
						"being enforced.", tc.construct, tc.exposure)
				}
				if !strings.Contains(err.Error(), "server-only") {
					t.Errorf("%s was refused, but NOT by the server-only gate -- so this case would "+
						"keep passing if the gate were removed and something else happened to "+
						"fail: %v", tc.construct, err)
				}
			})

			// The half that makes the annotation survivable rather than merely
			// strict. component/identity/recoverykey's executeServerOnly
			// stamps exactly this, on a context scoped to the one call. When
			// that stamp was missing, every case above still passed and the
			// entire feature was dead.
			t.Run("an internal-origin call passes the gate", func(t *testing.T) {
				if _, err := eng.Execute(auth.ContextWithInternalOrigin(ctx), tc.call); err != nil {
					if strings.Contains(err.Error(), "server-only") {
						t.Fatalf("%s refused an INTERNAL-origin call. Nothing can reach this "+
							"construct, so the break-glass credential cannot be minted, claimed "+
							"or redeemed -- the cluster has no route back in for its owner: %v",
							tc.construct, err)
					}
					// Anything else is this test's own fixture being wrong
					// (a bad arg, a missing row), not the property under test.
					t.Fatalf("%s failed for a non-gate reason -- fix the fixture, the gate is fine: %v",
						tc.construct, err)
				}
			})
		})
	}
}
