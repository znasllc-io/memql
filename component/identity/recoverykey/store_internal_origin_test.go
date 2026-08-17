package recoverykey

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// store_internal_origin_test.go -- the production half of the recovery-key
// @serverOnly contract.
//
// EVERY construct this store issues is @serverOnly: the two reads
// (activeRecoveryKeys, recoveryKeyByHash) and all four writes
// (createRecoveryKeyIdentity, claimRecoveryKey, redeemRecoveryKey,
// deactivateRecoveryKey). The engine refuses a @serverOnly construct unless
// auth.OriginFromContext says the call is server-initiated, and the only place
// this package can say so is inside the store.
//
// It did not say so, and the consequence was not one broken call. It was the
// whole feature: the boot invariant could not read, so no cluster ever minted
// an owner recovery key; `memql recovery-key claim` exited 1; owner rotation
// failed; and the redeem path -- the break-glass sign-in the credential exists
// for -- could not resolve a presented key. Every v0.19.0 cluster booted with
// no break-glass route and a WARN nobody reads.
//
// WHY THE PACKAGE'S OWN TESTS DID NOT CATCH IT, which is the part worth
// keeping: mint_singleflight_db_test.go fakes the engine, deliberately and for
// a good reason (it is testing a Postgres advisory lock, and a fake engine is
// what makes the race window wide enough to observe). A fake engine has no
// @serverOnly gate. So the package had a Postgres-gated test that exercised
// the real lock against an engine that could not refuse it, and the refusal
// was the bug. That is why this test asserts what the STORE stamped rather
// than what the caller stamped -- an engine-side test cannot pin this half,
// because it stamps at its own call site and never routes through the store.
//
// Modelled on component/identity/workertoken/store_internal_origin_test.go,
// which pins the same property for the same reason (memql#3063).

// originRecordingEngine records the CallOrigin of every context it is handed,
// so a test can assert what the STORE stamped rather than what the test
// itself stamped. Satisfies identity.EngineExecutor.
//
// Distinct from this package's other fake (fakeEngine, in
// mint_singleflight_db_test.go) on purpose: that one models row state to
// exercise the mint race and answers only two constructs. This one models
// nothing and answers everything, because what it is watching is the context,
// not the query.
type originRecordingEngine struct {
	// sawInternal is one entry per Execute, in call order.
	sawInternal []bool
	// queries is the parallel list of query text, so a failure can name the
	// construct whose call was not stamped.
	queries []string
}

func (f *originRecordingEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.sawInternal = append(f.sawInternal, auth.OriginFromContext(ctx).IsInternal())
	f.queries = append(f.queries, q)
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: canonicalIdPrefix + "rk-a",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":     structpb.NewStringValue(canonicalIdPrefix + "rk-a"),
				"userId": structpb.NewStringValue("user-1"),
				"active": structpb.NewBoolValue(true),
			}},
		}}},
		Meta: &memqlengine.ResultMeta{},
	}, nil
}

// TestEveryStoreOperationStampsInternalOriginItself drives all six operations
// with a CLIENT-origin context -- the shape the redeem path really passes,
// since component/identity/http/webauthn_recovery.go calls Resolve on
// r.Context() from an unauthenticated HTTP request -- and asserts the engine
// saw internal every time.
//
// Table-driven over all six rather than spot-checking one: they became
// @serverOnly together (memql#3964/#3965/#3970) and a partial stamp is the
// failure mode most likely to be introduced later, when someone adds a
// seventh operation by copying a neighbour that happens to be the one without
// the stamp.
func TestEveryStoreOperationStampsInternalOriginItself(t *testing.T) {
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		// op is the store method under test.
		op string
		// construct is the @serverOnly DSL construct it issues, named so a
		// failure points at the annotation that does the refusing.
		construct string
		call      func(context.Context, *Store) error
	}{
		{"ActiveForUser", "activeRecoveryKeys", func(ctx context.Context, s *Store) error {
			_, err := s.ActiveForUser(ctx, "user-1")
			return err
		}},
		{"LookupByHash", "recoveryKeyByHash", func(ctx context.Context, s *Store) error {
			_, err := s.LookupByHash(ctx, "deadbeef")
			return err
		}},
		{"Create", "createRecoveryKeyIdentity", func(ctx context.Context, s *Store) error {
			return s.Create(ctx, "rk-a", "user-1", "hash", "system:identity-svc", "", DefaultLabel)
		}},
		{"Claim", "claimRecoveryKey", func(ctx context.Context, s *Store) error {
			return s.Claim(ctx, "rk-a", "", at)
		}},
		{"Redeem", "redeemRecoveryKey", func(ctx context.Context, s *Store) error {
			return s.Redeem(ctx, "rk-a", "203.0.113.7", at)
		}},
		{"Deactivate", "deactivateRecoveryKey", func(ctx context.Context, s *Store) error {
			return s.Deactivate(ctx, "rk-a")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			eng := &originRecordingEngine{}
			s := &Store{Engine: eng}

			// Explicitly client-stamped, not merely unstamped. OriginClient is
			// the zero value, so an unstamped context would pass a store that
			// stamps nothing IF the engine defaulted the other way -- it does
			// not, but saying "client" out loud makes the test's premise the
			// same as the redeem path's reality rather than a default.
			clientCtx := auth.ContextWithClientOrigin(context.Background())
			if err := tc.call(clientCtx, s); err != nil {
				t.Fatalf("%s: %v", tc.op, err)
			}

			if len(eng.sawInternal) == 0 {
				t.Fatalf("%s never reached the engine, so this case asserts nothing", tc.op)
			}
			for i, internal := range eng.sawInternal {
				if !internal {
					t.Fatalf("%s passed a NON-internal context to the engine (call %d: %s).\n"+
						"%s is @serverOnly, so the real engine refuses this and returns "+
						"`function %q is server-only and cannot be called by a client`. "+
						"With the stamp missing the whole break-glass credential is inert: the boot "+
						"invariant cannot read, so no cluster mints an owner recovery key; "+
						"`memql recovery-key claim` exits 1; and redeem cannot resolve a presented "+
						"key. The stamp inside %s is the only thing that makes the annotation "+
						"survivable; it is gone or it moved.",
						tc.op, i, eng.queries[i], tc.construct, tc.construct, tc.op)
				}
			}
		})
	}
}

// TestResolveStampsInternalOriginItself covers the redeem path's real entry
// point rather than the store method underneath it.
//
// Resolve is what component/identity/http/webauthn_recovery.go calls, on an
// UNAUTHENTICATED request context, and it reaches the engine through
// LookupByHash. Asserting LookupByHash alone would leave the possibility that
// Resolve grew its own engine call later; this pins the caller the wire
// actually reaches.
//
// It also documents the argument that makes a request-derived stamp defensible
// here: Resolve hashes the presented plaintext ITSELF, so the value that
// reaches the filter is a digest of a secret the caller had to possess. Naming
// it is a possession proof, not an identifier a caller can choose -- which is
// the opposite of workerTokensForUser's caller-supplied userId, and why this
// read needs no equivalent of that construct's caller-scope test.
func TestResolveStampsInternalOriginItself(t *testing.T) {
	eng := &originRecordingEngine{}
	s := &Store{Engine: eng}

	// A syntactically valid recovery key, so Resolve does not short-circuit
	// before the engine on the IsRecoveryKey check.
	plain, _, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, _, err := s.Resolve(auth.ContextWithClientOrigin(context.Background()), plain); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(eng.sawInternal) == 0 {
		t.Fatal("Resolve never reached the engine, so this test asserts nothing -- " +
			"Mint produced a value IsRecoveryKey rejected, or Resolve stopped reading")
	}
	for i, internal := range eng.sawInternal {
		if !internal {
			t.Fatalf("Resolve passed a NON-internal context to the engine (call %d: %s). "+
				"recoveryKeyByHash is @serverOnly, so every break-glass sign-in fails closed: "+
				"a valid key presented at POST /auth/webauthn/recovery/begin reads as invalid.",
				i, eng.queries[i])
		}
	}
}
