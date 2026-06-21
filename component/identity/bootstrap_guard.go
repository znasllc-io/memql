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
	// BootstrapActionSelfHeal — an owner user already exists but the
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
	HasOwnerUser(ctx context.Context) (bool, error)
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
//  2. owner user exists             -> SelfHeal  (claimed; stamp lost)
//  3. clusterSettings row exists    -> Suppress  (mid-claim; email sent)
//  4. none of the above             -> Send      (truly fresh)
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

	// An owner user is definitional proof the cluster was claimed. If
	// one exists but we got here (bootstrappedAt empty), the verifier's
	// stamp write was swallowed on a prior boot — self-heal it instead
	// of emailing.
	hasOwner, err := store.HasOwnerUser(ctx)
	if err != nil {
		return BootstrapActionSkip, err
	}
	if hasOwner {
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
