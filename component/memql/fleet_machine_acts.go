package memql

// The two acts on a machine that a CLIENT used to compose (epic memql#5327,
// design D2 and finding M-2).
//
// ===========================================================================
// WHY EITHER IS A BUILTIN RATHER THAN A MUTATION
// ===========================================================================
// Both write v1:worker:registration, which declares
// @rowAuthz(owner="ownerUserId", clusterOwner) -- the owned tier with the
// admin gate ORed in. That tier is right for reading somebody's fleet as an
// operator and wrong for these two writes, in two different ways:
//
//   REMOVE  is not one write. It is a registration revoke AND a credential
//           revoke, and the client made only the first -- leaving a machine
//           that kept its stream, kept heartbeating, and could re-register.
//           Two calls from a browser also means a window between them.
//
//   SHARE   is a CONSENT. The cluster-owner escape on that tier let an
//           operator lend hardware they do not own, and un-lend hardware
//           somebody else had lent. A caller-scoped filter cannot fix it,
//           because the caller IS the owner on the legitimate path -- what is
//           too wide is the tier.
//
// Both resolve the machine through `modelPullMachineFor`, the AUTHORIZED read
// (workersForUser under the caller's own actor). A machine that is not the
// caller's is not in the answer, so neither function has to be trusted to
// check, and the not-found and not-yours refusals are the same sentence --
// which is also what stops either one being an existence oracle for other
// people's machines.
//
// AND THEN THEY PART COMPANY ON EXACTLY ONE THING. Removal falls back to the
// cluster-wide read for a cluster owner, because offboarding somebody's laptop
// is an operator act the Fleet's operator view already implies. Sharing does
// not, because what that write contains is a person's consent. See
// machineToRevoke.

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// RevokeMachineConcept and SetSharingConcept name the virtual rows these acts
// answer with. Neither is persisted: the act's own effect is on
// v1:worker:registration and on the credential row, and what comes back is the
// receipt.
const (
	RevokeMachineConcept = "v1:worker:machineRevocation"
	SetSharingConcept    = "v1:worker:sharingDecision"
)

// SharingModeOwner and SharingModeCluster are the two halves of the owner's
// consent, spelled the way the mutation's enum spells them.
const (
	SharingModeOwner   = "owner"
	SharingModeCluster = "cluster"
)

// evaluateFleetRevokeMachineExpression serves the `fleetRevokeMachine`
// builtin: revoke one of the caller's machines, registration and credential
// together.
func (e *MemQLEngine) evaluateFleetRevokeMachineExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	registrationId := strings.TrimSpace(stringArg(args, "registrationId"))
	machine, err := e.machineToRevoke(ctx, registrationId)
	if err != nil {
		return nil, err
	}
	if machine.RevokedAt != "" {
		// Already removed. Answering the receipt rather than an error, because
		// a second click on a button whose row has already gone is not a
		// mistake anybody needs told about -- and the end state is the one
		// asked for.
		return singleVirtualRow(RevokeMachineConcept, registrationId, map[string]any{
			"machineId":         registrationId,
			"registrationState": "revoked",
			"credentialState":   "unchanged",
			"alreadyRevoked":    true,
			"sentence":          "That machine was already removed.",
		})
	}

	now := time.Now().UTC()
	reason := strings.TrimSpace(stringArg(args, "reason"))
	acting := strings.TrimSpace(actingUserFromContext(ctx))

	// ORDER IS ROW FIRST, and it is a ruling rather than an accident. The
	// registration write is the one the mesh broadcasts, and the watcher on
	// the replica holding the stream reads it and ends the connection. A
	// credential revoked first would leave a machine unable to reconnect while
	// its row still advertised it as routable -- the same split this act
	// exists to close, pointing the other way.
	revoke, err := langparser.RenderCall("revokeWorker", map[string]any{
		"registrationId": registrationId,
		"revokedAt":      now.Format(time.RFC3339Nano),
		"revokedBy":      acting,
		"revokeReason":   reason,
	})
	if err != nil {
		return nil, fmt.Errorf("fleetRevokeMachine: render the revoke: %w", err)
	}
	if _, err := e.Execute(ctx, revoke); err != nil {
		return nil, fmt.Errorf("fleetRevokeMachine: revoke the registration: %w", err)
	}

	// THE CREDENTIAL IS BEST EFFORT, AND THE RECEIPT SAYS SO. The row is
	// already revoked, so the machine is out of every routing decision and its
	// stream is ending; failing the whole act here would tell an operator that
	// nothing happened when half of it had, which is the worst of the three
	// possible answers. What they get instead is a receipt naming exactly
	// which half did not land.
	credentialState := "revoked"
	sentence := "Removed. The machine is out of the fleet and its credential no longer connects."
	if identityId := strings.TrimSpace(machine.IdentityId); identityId == "" {
		credentialState = "not_found"
		sentence = "Removed from the fleet. No credential was bound to this registration, so there was none to revoke."
	} else if err := e.revokeWorkerCredential(ctx, identityId); err != nil {
		credentialState = "revoke_failed"
		sentence = "Removed from the fleet, but its credential could not be revoked. The machine cannot be routed to, and it will be disconnected -- but revoke the credential from Settings before trusting that it cannot reconnect."
		if e.Component != nil && e.Logger != nil {
			e.Logger.Warn("fleetRevokeMachine: the registration was revoked but its credential was not",
				"registration_id", registrationId,
				"identity_id", identityId,
				"error", err,
			)
		}
	}

	return singleVirtualRow(RevokeMachineConcept, registrationId, map[string]any{
		"machineId":         registrationId,
		"registrationState": "revoked",
		"credentialState":   credentialState,
		"alreadyRevoked":    false,
		"revokedAt":         now.Format(time.RFC3339),
		"sentence":          sentence,
	})
}

// machineToRevoke resolves the machine a removal names: through the caller's
// OWN machines first, and through the cluster-wide read only for a cluster
// owner.
//
// REMOVAL KEEPS THE CLUSTER-OWNER ARM AND SHARING DOES NOT, which is the one
// place these two acts part company (epic memql#5327). The registration
// concept declares @rowAuthz(owner="ownerUserId", clusterOwner), and for
// REMOVAL the second arm is right: offboarding somebody's laptop is an
// operator act, the Fleet's operator view already lists every machine in the
// cluster, and withdrawing that would be a capability lost to a fix for a
// different problem. For SHARING it is wrong, because the content of that
// write is a person's consent and nobody else's to give.
//
// THE OWNED READ RUNS FIRST AND UNCONDITIONALLY, so an ordinary user's
// refusal is produced with no cross-owner read happening at all -- and a
// machine that is not theirs answers exactly as a made-up id does.
func (e *MemQLEngine) machineToRevoke(ctx context.Context, registrationId string) (modelPullMachine, error) {
	machine, ownErr := e.modelPullMachineFor(ctx, registrationId)
	if ownErr == nil {
		return machine, nil
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return modelPullMachine{}, ownErr
	}
	// The operator path. allWorkersWithStatus gates ITSELF on
	// actor.isClusterOwner, so this read answers nothing for anybody else --
	// the check above is what keeps a non-owner's refusal cheap and identical,
	// not what makes this safe.
	call, err := langparser.RenderCall("allWorkersWithStatus", map[string]any{})
	if err != nil {
		return modelPullMachine{}, fmt.Errorf("fleetRevokeMachine: render the operator read: %w", err)
	}
	res, err := e.Execute(ctx, call)
	if err != nil {
		return modelPullMachine{}, fmt.Errorf("fleetRevokeMachine: read the cluster's machines: %w", err)
	}
	want := trimConceptPrefix(registrationId)
	for _, row := range modelPullRows(res.OutputPayload()) {
		rowId := mapString(row, "id")
		if rowId != registrationId && trimConceptPrefix(rowId) != want {
			continue
		}
		return modelPullMachine{
			RegistrationId:  registrationId,
			OwnerUserId:     mapString(row, "ownerUserId"),
			IdentityId:      mapString(row, "identityId"),
			DisplayName:     mapString(row, "displayName"),
			Name:            mapString(row, "name"),
			ConnectedNodeId: mapString(row, "connectedNodeId"),
			LastSeenAt:      mapString(row, "lastSeenAt"),
			RevokedAt:       mapString(row, "revokedAt"),
		}, nil
	}
	// THE OWNED REFUSAL, not an operator-flavoured one. A cluster owner who
	// names an id that exists nowhere and one who names a machine that does
	// not exist get the same sentence -- and it is the sentence everybody else
	// gets too.
	return modelPullMachine{}, ownErr
}

// revokeWorkerCredential deactivates the worker-token identity bound to a
// registration.
//
// It renders revokeWorkerTokenIdentity directly rather than reaching for
// component/identity/workertoken, because component/memql cannot import a
// package that imports it. The mutation is not @serverOnly and
// v1:identity:identity declares no row-authz tier, so the caller's own context
// carries it -- no stamp, and nothing here widens what the caller can reach.
func (e *MemQLEngine) revokeWorkerCredential(ctx context.Context, identityId string) error {
	call, err := langparser.RenderCall("revokeWorkerTokenIdentity", map[string]any{
		"identityId": trimConceptPrefix(identityId),
	})
	if err != nil {
		return fmt.Errorf("render the credential revoke: %w", err)
	}
	if _, err := e.Execute(ctx, call); err != nil {
		return fmt.Errorf("revoke the credential: %w", err)
	}
	return nil
}

// evaluateFleetSetSharingExpression serves the `fleetSetSharing` builtin: the
// OWNER's half of a machine's sharing consent, written through the caller's
// own machines.
func (e *MemQLEngine) evaluateFleetSetSharingExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	mode := strings.TrimSpace(stringArg(args, "mode"))
	// REFUSED rather than read as `owner`. The two values mean opposite things
	// and a typo has to be told about: silently keeping a machine private on a
	// misspelling is safe, and silently doing anything else is not -- but an
	// owner who typed `clustre` and saw nothing happen would try again, and
	// eventually conclude the feature is broken.
	if mode != SharingModeOwner && mode != SharingModeCluster {
		return nil, modelPullRefusal{
			Code:    "invalid_sharing_mode",
			Message: fmt.Sprintf("sharing mode must be %q or %q, not %q", SharingModeOwner, SharingModeCluster, mode),
		}
	}
	registrationId := strings.TrimSpace(stringArg(args, "registrationId"))
	machine, err := e.modelPullMachineFor(ctx, registrationId)
	if err != nil {
		return nil, err
	}
	if machine.RevokedAt != "" {
		return nil, modelPullRefusal{
			Code:    "machine_revoked",
			Message: "that machine has been removed from the fleet, so there is nothing to share",
		}
	}

	call, err := langparser.RenderCall("setWorkerSharing", map[string]any{
		"registrationId": registrationId,
		"mode":           mode,
	})
	if err != nil {
		return nil, fmt.Errorf("fleetSetSharing: render the write: %w", err)
	}
	if _, err := e.Execute(ctx, call); err != nil {
		return nil, fmt.Errorf("fleetSetSharing: %w", err)
	}

	// THE SENTENCE NAMES THE OTHER HALF, because a consent that is half given
	// looks exactly like one that is not given at all. An owner who turns
	// sharing on and sees nothing happen has no way to learn from this surface
	// that the machine's own policy.yaml is the thing still saying no.
	sentence := "Kept to you. Nobody else's work will run on this machine."
	if mode == SharingModeCluster {
		sentence = "Offered to the cluster. The machine itself has to agree too -- set `inference.serve: cluster` in its policy.yaml -- and until it does, nothing runs on it."
	}
	return singleVirtualRow(SetSharingConcept, registrationId, map[string]any{
		"machineId": registrationId,
		"mode":      mode,
		"sentence":  sentence,
	})
}
