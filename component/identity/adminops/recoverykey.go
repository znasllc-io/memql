package adminops

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/recoverykey"
)

// recoverykey.go -- rotating the owner recovery key as a signed-in owner
// (memql#3970).
//
// # WHY THIS EXISTS BESIDE THE CLI
//
// `memql recovery-key claim` (memql#3969) already rotates, from inside the
// pod. That path is for the operator who CANNOT sign in. This one is for the
// operator who can, and the two are not redundant:
//
//   - Rotating on suspicion of a leak is urgent, and the person who notices is
//     usually signed in. Making them find a kubeconfig first is the difference
//     between rotating now and rotating later.
//   - An install that ended without the key being claimed leaves a live
//     credential nobody holds. That is not a lockout, but it is a break-glass
//     route that will not work when it is reached for, and the fix should be a
//     button rather than a runbook.
//
// # THE PREDECESSOR IS RETIRED, NOT DELETED
//
// active=false, with rotatedFrom on the new row recording the lineage, so an
// operator can follow the chain of custody without reconstructing it from
// timestamps. And the replacement is created BEFORE the predecessor is
// retired: the other order leaves a window with no live key at all, which is
// the one state this credential must never be in.
//
// # THE PLAINTEXT RIDES THE RESULT AND NOT THE AUDIT DETAIL
//
// Exactly as IssueEnrolmentLink's does, and for the same reason: the audit
// trail records that a rotation happened, to whom, by whom and when. Recording
// the key itself would put a live owner credential into an append-only log
// that is deliberately hard to redact.

// RotateRecoveryKey mints a replacement owner recovery key and retires the
// current one.
//
// userId may be empty when the cluster has exactly one owner. With several it
// is required: picking one would hand the caller a credential for an account
// they did not name.
func (s *Service) RotateRecoveryKey(ctx context.Context, userId string) Result {
	target := strings.TrimSpace(userId)
	detail := map[string]any{"userId": target}

	act, refusal, allowed := s.authorize(ctx, "rotating the recovery key", detail)
	if !allowed {
		return refusal
	}

	store := &identity.Store{Engine: s.Engine, Logger: s.Logger}
	if target == "" {
		owners, err := store.OwnerUserIds(ctx)
		if err != nil {
			return s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, "", "",
				detail, "", fmt.Errorf("resolve cluster owner: %w", err))
		}
		switch len(owners) {
		case 0:
			return fail(CodeFailedPrecondition, s.emit(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated",
				act, "", "", detail, identity.AuditOutcomeFailure, "no_cluster_owner"),
				"identity admin: this cluster has no owner, so there is no recovery key to rotate")
		case 1:
			target = owners[0]
			detail["userId"] = target
		default:
			return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated",
				act, "", "", detail, identity.AuditOutcomeFailure, "ambiguous_owner"),
				fmt.Sprintf("identity admin: this cluster has %d owners; name the one whose recovery key to rotate", len(owners)))
		}
	}

	// The target must exist. Rotating against a typo'd id would mint a live
	// credential bound to an account that is not there -- a key that can never
	// be redeemed, discovered only when somebody reaches for it.
	user, err := s.userById(ctx, target)
	if err != nil || user == nil {
		return s.notFound(ctx, "recovery_key_rotated", act, target, detail, err)
	}

	recStore := &recoverykey.Store{Engine: s.Engine, Logger: s.Logger}

	// writeCtx carries the system CREDENTIAL actor, and every WRITE below uses
	// it while every audit emit keeps `ctx` -- the same split adminops.go makes
	// for node-token revoke, and for the same reason twice over.
	//
	// It is REQUIRED: `recovery_key` is a machineCredentialIdentityType, so the
	// memql#2513 guard admits a write only from role=="system" or a
	// "system:"-prefixed actor. The caller here is a signed-in owner or admin,
	// so without this every rotation is refused -- the guard does not care that
	// the human is privileged, only that a human is not a system actor. (Nor
	// would ContextWithSystemActor help: it stamps role="owner" plus an email,
	// and ActorFromToken prefers email over subject, so it fails both arms too.)
	//
	// And it is CORRECT rather than a workaround: who rotated the key is
	// already recorded twice, in the audit event this function emits under the
	// caller's own context and in the row's `mintedBy`, which is passed
	// act.userID below. Nothing is lost by the row's createdBy naming the
	// credential service, which is what every other machine credential does.
	writeCtx := identity.ContextWithSystemCredentialActor(ctx)

	live, err := recStore.ActiveForUser(ctx, target)
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target,
			user.PrimaryEmail, detail, "", fmt.Errorf("read active recovery keys: %w", err))
	}

	plain, hash, err := recoverykey.Mint()
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target,
			user.PrimaryEmail, detail, "", fmt.Errorf("recovery key mint: %w", err))
	}
	newId, err := recoverykey.NewId()
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target,
			user.PrimaryEmail, detail, "", fmt.Errorf("recovery key id mint: %w", err))
	}

	rotatedFrom := ""
	if len(live) > 0 {
		rotatedFrom = recoverykey.CanonicalId(live[0].ID)
		detail["rotatedFrom"] = rotatedFrom
	}

	// CREATE FIRST. Retiring the predecessor before the replacement exists
	// would leave a window with no live recovery key, and a failure in between
	// would leave it that way permanently.
	if err := recStore.Create(writeCtx, newId, target, hash, act.userID, rotatedFrom, recoverykey.DefaultLabel); err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target,
			user.PrimaryEmail, detail, "", err)
	}
	// Claimed at the moment of rotation: the caller is being shown the value
	// right now, so the row must stop being freely re-mintable. Skipping this
	// would let the boot invariant replace a key the operator had already
	// written down.
	if err := recStore.Claim(writeCtx, newId, "", s.Now()); err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target,
			user.PrimaryEmail, detail, "", fmt.Errorf("stamp claimed: %w", err))
	}
	for _, old := range live {
		if err := recStore.Deactivate(writeCtx, old.ID); err != nil {
			// The replacement is already live and already shown. Failing the
			// whole call here would tell the caller the rotation did not
			// happen when it did, and they would rotate again -- so this is
			// reported as an error on a result that still carries the key.
			res := s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target,
				user.PrimaryEmail, detail, "", fmt.Errorf("retire previous key %s: %w", old.ID, err))
			res.RecoveryKey = plain
			return res
		}
	}
	detail["recoveryKeyId"] = recoverykey.CanonicalId(newId)

	res := s.finish(ctx, identity.AuditCategoryAdmin, "recovery_key_rotated", act, target, user.PrimaryEmail,
		detail, "Recovery key rotated. Copy it now -- it is not shown again.", nil)
	res.RecoveryKey = plain
	return res
}
