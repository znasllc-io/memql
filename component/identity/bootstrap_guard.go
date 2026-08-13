package identity

import (
	"context"
	"errors"
)

// BootstrapAction is the decision returned by EvaluateAutoBootstrap:
// what the identity auto-bootstrap path should do on this boot.
type BootstrapAction int

const (
	// BootstrapActionSkip — the cluster is already fully bootstrapped
	// (bootstrappedAt stamped). Nothing to do; do NOT email.
	BootstrapActionSkip BootstrapAction = iota
	// BootstrapActionSelfHeal — the owner has ALREADY AUTHENTICATED but the
	// bootstrappedAt stamp is missing (the verifier's stamp write was
	// swallowed on a prior boot, memql#1864). Reconcile by stamping;
	// do NOT email — the cluster was already claimed.
	BootstrapActionSelfHeal
	// BootstrapActionSuppress — clusterSettings row already exists but
	// no owner has claimed yet. The claim email was already sent on the
	// boot that created the row; re-sending would spam the owner on
	// every restart (memql#1829). Do NOT email.
	BootstrapActionSuppress
	// BootstrapActionSend — a genuinely fresh cluster: no bootstrap
	// stamp, no owner user, no clusterSettings row, and every read
	// succeeded. Persist the row + send exactly one claim email.
	BootstrapActionSend
)

// BootstrapGuardStore is the narrow read surface EvaluateAutoBootstrap
// needs. *Store satisfies it; tests supply a fake. All three reads are
// error-returning so a transient DB failure surfaces as "unknown"
// instead of being swallowed into a false that re-triggers the email.
type BootstrapGuardStore interface {
	IsClusterBootstrappedE(ctx context.Context) (bool, error)
	// HasClaimedOwner, NOT HasOwnerUser (memql#3591). See EvaluateAutoBootstrap.
	HasClaimedOwner(ctx context.Context) (bool, error)
	ReadClusterSettings(ctx context.Context) (*ClusterSettingsRow, error)
}

// EvaluateAutoBootstrap decides what the identity auto-bootstrap path
// should do, gating the one-time "claim the cluster" email on whether
// the cluster was EVER claimed rather than solely on bootstrappedAt
// (memql#1864).
//
// Fail-safe contract: if ANY of the three reads errors, the cluster
// state cannot be determined, so the function returns a non-nil error
// and the caller MUST NOT send the email. "Can't tell" is treated as
// "don't spam the owner", never as "fresh cluster -> send".
//
// Ordering of the checks is deliberate:
//  1. bootstrappedAt set            -> Skip      (steady state)
//  2. owner has authenticated       -> SelfHeal  (claimed; stamp lost)
//  3. clusterSettings row exists    -> Suppress  (mid-claim; email sent)
//  4. none of the above             -> Send      (truly fresh)
//
// STEP 2 ASKS ABOUT CREDENTIALS, NOT ROWS (memql#3591). It used to read
// HasOwnerUser -- "an owner user exists" -- as definitional proof the cluster was
// claimed, which was sound only while the one way a user row could appear was
// somebody logging in. The install now writes the owner row when it bootstraps
// from env, so that the cluster has a named owner a passkey-enrolment link can be
// minted for; a row written that way is not a claim, and reading it as one would
// stamp bootstrappedAt before anybody had claimed anything -- marking the cluster
// claimed, taking /setup away as a fallback, and doing both silently.
//
// An owner holding a magic-link or passkey identity has authenticated, by either
// route; an owner holding none has never signed in. That is the fact this step
// always wanted.
func EvaluateAutoBootstrap(ctx context.Context, store BootstrapGuardStore) (BootstrapAction, error) {
	if store == nil {
		return BootstrapActionSkip, errors.New("identity: EvaluateAutoBootstrap: nil store")
	}

	bootstrapped, err := store.IsClusterBootstrappedE(ctx)
	if err != nil {
		return BootstrapActionSkip, err
	}
	if bootstrapped {
		return BootstrapActionSkip, nil
	}

	// An owner who has AUTHENTICATED is definitional proof the cluster was
	// claimed. If one exists but we got here (bootstrappedAt empty), the
	// verifier's stamp write was swallowed on a prior boot — self-heal it
	// instead of emailing.
	claimed, err := store.HasClaimedOwner(ctx)
	if err != nil {
		return BootstrapActionSkip, err
	}
	if claimed {
		return BootstrapActionSelfHeal, nil
	}

	existing, err := store.ReadClusterSettings(ctx)
	if err != nil {
		return BootstrapActionSkip, err
	}
	if existing != nil {
		return BootstrapActionSuppress, nil
	}

	return BootstrapActionSend, nil
}
