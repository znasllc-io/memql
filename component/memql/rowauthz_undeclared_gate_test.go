package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// rowauthz_undeclared_gate_test.go -- memql#3173, carrying memql#3077
// (the ratchet) and memql#3135 (the reason slot).
//
// # THE LIST BELOW MAY ONLY SHRINK
//
// `@rowAuthz` (memql#2920) makes a concept DECLARE who may see its rows,
// and every downstream phase -- shadow mode, the owner-stamping gate,
// enforcement -- reaches only the concepts that declare one. Everything
// else is not "safe" and not "unchanged"; it is unmeasured, and shadow
// mode's own report already says so in those words:
//
//	--- NOT MEASURABLE: concept declares no tier ---
//	  These are not 'no change' -- they are 'not measured'.
//
// That undeclared population is where the credential surface lives:
// `badgesForUser`, `authSessionByRefreshTokenHash`, `auditEventsByActor`,
// `activeUsers`, `patIdentityByKeyHash`. The coverage is inverted against
// risk, and before this file the only signal over it was a boot-time
// warning -- a warning that has been true for the same ~170 constructs
// for months, which is not a signal, it is wallpaper.
//
// So the debt is enumerated once, as it stood the day the gate landed,
// and the gate then fails in BOTH directions:
//
//   - ADDING an entry means a new query was written over a concept that
//     still declares nothing. That is new debt on the exact surface this
//     epic exists to close. Declare a tier on the concept instead; if an
//     entry is genuinely unavoidable it must carry its own filed issue as
//     its reason, which is what makes it visibly different from the seed.
//   - REMOVING an entry is the point of the file, and happens by
//     declaring a tier on the concept (or by deleting the query). The
//     gate then fails until the entry is deleted, because a stale entry
//     silently suppresses that construct forever. The sibling gate states
//     the rule: "a stale exemption is as bad as a missing gate"
//     (ownerGateExemptions, memql#2982).
//
// # THIS FILE DECLARES A TIER ON NOTHING
//
// Deliberately, and it is not an oversight to be tidied up later. Each
// identity entry below is a real authorization judgment -- `activeUsers`
// and `badgesForUser` are not obviously owner-scoped, and several of
// these reads are legitimately cross-user admin reads. Burying dozens of
// security decisions inside a tooling change is precisely the outcome
// this gate exists to make impossible to do quietly.
//
// # WHY IT DERIVES FROM THE LOADED REGISTRY
//
// The population is read off the loaded FunctionRegistry and the loaded
// concept registry, never off a scan of the .memql source, so the list is
// a pin on the tree rather than a second hand-maintained copy of it. A
// source scanner would have to re-derive which construct binds which
// concept and whether that concept's `@rowAuthz` survived the parse; the
// loader already carries the outcome. That is the memql#2875 lesson,
// applied here for the same reason OwnerFieldProvenance applies it.

// undeclaredGrandfatherReason is what every seed entry carries.
//
// It says "population, not triage" because that is what happened: the set
// was enumerated in one pass and no single entry on it was individually
// judged. A per-entry fiction ("admin read, intentional") would record a
// claim nobody actually made. The shared marker also makes a NEW entry
// visibly different at a glance -- a new one has to name its own issue.
const undeclaredGrandfatherReason = "memql#3173 seed -- grandfathered as a population, not individually triaged"

// undeclared3461ByIdReason covers the by-id read memql#3461 added so the
// campaign feedback parser can fetch the staged webhook body an automation
// hands it.
//
// Deliberately NOT carrying the grandfather marker: it was added after the
// seed and names its own issue, which is the distinction that marker exists
// to make visible.
//
// Why it is listed rather than paid off. Declaring a tier on
// v1:platform:inboundRequest is a decision about the INBOUND DELIVERY
// feature (memql#2957) -- it changes the result set of the two reads above
// and of every product automation draining them -- and making it as a side
// effect of a campaigns task would be exactly the kind of quiet scope change
// this gate is here to surface. The read itself is strictly narrower than
// the listed `inboundRequestsByStatus`, which returns pages of the same rows
// under the same absent tier: this one returns at most one, by an id the
// caller already holds.
const undeclared3461ByIdReason = "memql#3461 -- by-id read of a staged webhook; the tier decision belongs to inbound delivery (memql#2957), and this is strictly narrower than the listed inboundRequestsByStatus"

// undeclared3178SelfScopedReason covers the two constructs memql#3178
// introduced while splitting the per-user credential lists off a
// caller-supplied id.
//
// They are on this list, and they are deliberately NOT carrying the
// grandfather marker, because they are not grandfathered -- they were added
// after the seed and each names its own issue, which is exactly the
// distinction the marker exists to make visible.
//
// Being listed here does not mean they are unscoped. Both filter on
// `userId==actor.userId`, so they are strictly narrower than the
// `userId==args.userId` constructs they replaced. What keeps them on the
// list is the concept: `v1:identity:identity` still declares no `@rowAuthz`
// tier, so the engine measures nothing about them and this gate's whole
// point is that unmeasured is not the same as safe.
//
// Declaring the tier is NOT this epic's job -- epic rowauthz-enforcement
// Decision D holds that each of the 48 identity declarations is its own
// authorization judgment and burying them inside a tooling change is the
// outcome this gate exists to prevent.
const undeclared3178SelfScopedReason = "memql#3178 -- self-scoped on actor.userId; v1:identity:identity still declares no tier (epic Decision D)"

// undeclared3322AccountTokenReason covers the two constructs memql#3322 added
// for the account-token surface in the MemQL Portal.
//
// Same shape as the memql#3178 pair above, and listed for the same single
// reason: `v1:identity:identity` declares no `@rowAuthz` tier, so the engine
// measures nothing about a read over it. Both queries carry their own
// predicates and neither is an open read -- but a per-query predicate is
// precisely the author-enforced thing the tier exists to stop depending on,
// and the next query over this concept starts from zero again.
//
// They carry their OWN issue rather than the grandfather marker, because they
// were added long after the seed. memql#3349 is that issue: it inventories
// every read of the credential concept, including the ones that run BEFORE an
// actor is resolved (magic-link verification, PAT/worker/service-account/node
// token checks), which is why the tier could not simply be declared here --
// enforceRowAuthzOnPlan gives no cluster-owner escape, so a wrong tier on this
// concept fails auth cluster-wide rather than leaking.
const undeclared3322AccountTokenReason = "memql#3349 -- account-token reads; v1:identity:identity still declares no tier"

// undeclared3324NodeTokenAdminReason covers nodeTokenIdentitiesAdmin, added by
// memql#3324 when the server-rendered /admin/* console was retired into the
// portal SPA.
//
// It postdates the seed and names its own issue rather than carrying the
// grandfather marker, for the same reason as the two pairs above.
//
// Why it exists at all: the operator listing of node credentials was
// `nodeTokenIdentities`, which is `@serverOnly` and so answers a browser caller
// with a refusal. The templ console reached it from inside the identity binary;
// the SPA cannot. The alternatives were to drop the surface, to open
// `nodeTokenIdentities` to clients (which would widen a `@serverOnly` read to
// every caller), or to add a sibling that is client-reachable but role-gated.
// This is the third.
//
// What narrows it is `requiresOwnerOrAdmin` as a top-level conjunct, evaluated
// in-process against the auth envelope -- the same gate memql#2860 put on
// userById. `actor.userId` is NOT the alternative: a node_token row's `userId`
// is the synthetic bootstrap user rather than any reader, so an owner tier
// would return a confidently empty answer to the very admin the surface is for.
// It also projects the credential-free `nodeTokenSummary` shape, so `keyHash`
// never leaves the engine.
//
// It is on this list anyway, and that is the point the sibling reasons make:
// a per-query predicate is exactly the author-enforced thing the tier exists to
// stop depending on, and `v1:identity:identity` still declares no tier.
const undeclared3324NodeTokenAdminReason = "memql#3324 -- role-gated node-credential listing for the portal; v1:identity:identity still declares no tier"

// undeclared3217SeedSweepReason covers usersForSeedSweep, added by memql#3217
// so the startup per-user seed sweep reads a COMPLETE user set instead of
// activeUsers' newest-50 page.
//
// It is on this list and deliberately not carrying the grandfather marker: it
// postdates the seed and names its own issue, which is the distinction the
// marker exists to draw.
//
// Being listed here is not a claim that it is unscoped debt of the usual kind.
// It cannot be caller-scoped at all -- it runs under the seed materializer's
// system actor at boot, where there is no requesting user for actor.userId to
// name, and a filter naming one would evaluate against an empty actor and
// sweep nobody (the same circularity that keeps activeUsers @serverOnly rather
// than self-scoped, #2800/#2883). What keeps it here is the concept:
// v1:identity:user declares no `@rowAuthz` tier, so the engine measures
// nothing about it, and unmeasured is not the same as safe. Its projection is
// userIdRef -- row.id alone, no @pii field -- which bounds the exposure but
// does not measure it.
const undeclared3217SeedSweepReason = "memql#3217 -- system-actor startup sweep, uncaller-scopable by construction; v1:identity:user still declares no tier (epic Decision D)"

// undeclared3406PasskeyReason covers the two constructs memql#3406 added for
// the WebAuthn passkey variant.
//
// They postdate the seed and name their own issue rather than carrying the
// grandfather marker, for the same reason as the pairs above.
//
// `oidcIdentityBySubject` (memql#4611) joins them for the same reason and is
// the plainest pre-actor case of the three: it runs INSIDE the OIDC callback,
// between verifying an id token and deciding whether the person it names may be
// admitted at all -- so there is not merely no AccessContext, there is not yet a
// decision that this person has an account here. The row it returns holds no
// secret: the `oidc` variant stores the provider's assertion, not a credential.
//
// They are two different shapes and both end up here for the one reason the
// whole list exists: `v1:identity:identity` declares no `@rowAuthz` tier.
// `passkeysForSelf` filters `userId==actor.userId`, so it is the same
// self-scoped shape as `badgesForSelf`. `passkeyByCredentialId` is pre-actor
// in the strongest sense on this list -- at login (memql#3407) a discoverable
// assertion arrives with no user hint and no authenticated caller, so there is
// no `actor.userId` in existence for a tier to compare against, let alone a
// correct one. Both are classified in the memql#3349 credential-read inventory.
const undeclared3406PasskeyReason = "memql#3406 -- passkey reads (one self-scoped, one pre-actor); v1:identity:identity still declares no tier"

const undeclared4611OidcReason = "memql#4611 -- upstream-provider link lookup, pre-actor: it runs inside the OIDC callback before this person is known to have an account here, so there is no actor.userId to compare against; v1:identity:identity still declares no tier"

// undeclared3410DeviceCodeReason covers the two reads over
// `v1:identity:deviceCode`, the RFC 8628 device-authorization row added by
// memql#3410.
//
// They postdate the seed and name their own issue rather than carrying the
// grandfather marker, for the same reason as the entries above.
//
// Why a tier could not simply be declared, and this is a property of the
// grant rather than a deferral:
//
//   - `deviceCodeByDeviceCodeHash` is a PRE-ACTOR read in the same sense as
//     the `*ByKeyHash` credential lookups. The polling device holds no
//     session -- getting one is the entire point of the flow -- so there is
//     no `actor.userId` for an owner tier to compare against, and
//     `enforceRowAuthzOnPlan` has no read-side escape. An owner tier here
//     would AND `approvedByUserId == ""` into every poll and the grant would
//     never complete.
//   - `deviceCodeByUserCodeHash` DOES run for a signed-in human, and is
//     still not owner-scopable: the row is minted by a device with no user
//     attached, so `approvedByUserId` is empty at exactly the moment the
//     verification page has to find it. The field names the eventual
//     approver, not a pre-existing owner -- the same "the field is not an
//     ownership claim" finding memql#3349 records for the credential
//     concept.
//
// What bounds the two reads instead is the credential itself: each is a
// lookup by the SHA-256 digest of an unguessable secret (a 256-bit
// device_code, a ~50-bit user_code), returning at most one row, behind a
// per-IP limiter on both entry points. That is the same shape as
// `workerPairingCodeByHash` above, and it is a bound, not a measurement --
// which is why the entries are here.
const undeclared3410DeviceCodeReason = "memql#3410 -- device-grant credential lookups; the polling read is pre-actor and approvedByUserId is empty when the page read runs, so no owner tier fits"

// undeclared3408EnrolmentReason covers enrolmentTokenByHash, the /enroll
// redeem lookup memql#3408 added.
//
// It postdates the seed and names its own issue rather than carrying the
// grandfather marker, for the same reason as the entries above.
//
// It is on this list because it CANNOT be caller-scoped, not because nobody
// got around to scoping it. The enrolment token exists precisely so that a
// person with NO credential can obtain their first one -- the token IS the
// credential, and the read that validates it runs before any actor exists. An
// owner tier on v1:identity:enrolmentToken would compare the row's userId
// against an empty actor.userId and return nothing, turning every redeem into
// a confident "invalid link"; enforceRowAuthzOnPlan has no cluster-owner
// escape on the read path, so a wrong tier here fails the flow closed rather
// than leaking. The same circularity keeps magicLinkRequest's and
// workerPairingCode's lookups untiered.
//
// What bounds it instead is arithmetic rather than authorship: the filter is
// equality on a SHA-256 digest of 32 CSPRNG bytes, so the read resolves to at
// most one row and only for a caller who already holds the plaintext.
const undeclared3408EnrolmentReason = "memql#3408 -- /enroll redeem lookup; pre-actor by construction (the token IS the credential), so no owner tier can be compared against"

// undeclared4301MagicLinkByIdReason covers the by-id read the device-bound
// magic-link flow added.
//
// v1:identity:magicLinkRequest CANNOT declare a tier, for the reason its two
// existing reads were grandfathered under and one more that is specific to it.
// The pre-actor argument first: every read of this row happens BEFORE anyone is
// authenticated -- the poll and the finish are STEPS OF SIGNING IN, not things
// a signed-in person does -- so an owner tier would compare the row against
// actor.userId == "" and match nothing, turning every sign-in into a silent
// "invalid". The additional one: the row has no user pointer to scope BY. A
// magic-link request names an EMAIL ADDRESS, and on a first-time registration
// no v1:identity:user exists for it until the link is consumed, so there is no
// field an owner tier could name even in principle.
//
// What authorizes the read instead is the memql_ml binding cookie, compared
// against the row's bindingHash in Go before the handler acts on anything --
// and the query carries @serverOnly, so it is not reachable from the wire at
// all.
// undeclared4306SelfSessionsReason covers the self-service sessions read.
//
// THE CONSTRUCT IS ALREADY CALLER-SCOPED -- it filters userId==actor.userId
// and takes no argument at all, so there is no way to point it at another
// person. What is missing is a tier on the CONCEPT, and v1:identity:authSession
// cannot carry one: authSessionByTokenHash is the auth middleware's hot path
// and authSessionByRefreshTokenHash is rotation's, both PRE-ACTOR by
// construction. An owner tier would AND userId==actor.userId into the lookup
// that BUILDS the actor, so every authenticated request would fail to find its
// own session. It is the same shape as v1:identity:identity's recorded
// decision, one concept over.
//
// The right fix is the one written on v1:identity:identity: a read-path escape
// mirroring rowAuthzWriteEscape, so the pre-actor reads survive injection. Until
// that exists, this construct's own filter is the enforcement, and it is a
// stronger one than most entries here can claim.
const undeclared4306SelfSessionsReason = "memql#4306 -- self-scoped by its own filter (userId==actor.userId, no arguments); the CONCEPT cannot carry a tier because authSessionByTokenHash is the pre-actor read that builds the actor"

// undeclared4734SessionsForSubjectAdminReason covers the operator read
// memql#4734 added so the MemQL OS Users app can show how many sessions a
// person currently has.
//
// The CONCEPT still cannot carry a tier, for the reason already written on
// undeclared4306SelfSessionsReason: authSessionByTokenHash is the PRE-ACTOR
// read that builds the actor, so an owner tier would compare every session
// against actor.userId == "" and match nothing -- every request in the cluster
// would fail to authenticate. Declaring a tier to satisfy this gate would
// break the thing the gate is protecting.
//
// So the query gates ITSELF, and it is a new query rather than a reuse of
// authSessionsForSelfIncludingRevoked precisely because of that. Its sibling
// is a SERVER read -- two Go callers in the revoke handlers, scoped to the
// caller with no argument at all since memql#4768. This one is
// reached from a browser with an id the reader clicked, so it takes
// requiresOwnerOrAdmin as a top-level conjunct and projects
// authSessionAdminSummary, which has no token digests in it at all.
// undeclared4768OwnSessionsReason covers the read behind the two revoke
// handlers, after memql#4768 caller-scoped it.
//
// It was `authSessionsForSubject(subject: ...)` and carried the grandfather
// marker -- filtered on a caller-supplied id, with no role gate and no
// @serverOnly, so any signed-in caller could read anyone's sessions. It now
// takes no argument at all and filters `subject==actor.userId`, so there is
// nothing left to enumerate: the caller IS the scope.
//
// The CONCEPT still cannot carry a tier (see
// undeclared4306SelfSessionsReason: authSessionByTokenHash is the pre-actor
// read that builds the actor), and @serverOnly is unreachable because both
// callers live in component/grpc, which may never stamp internal origin --
// that gap is memql#4769. Caller-scoping is what closes this one without
// depending on either.
const undeclared4768OwnSessionsReason = "memql#4768 -- caller-scoped with no argument (subject==actor.userId), so there is nothing to enumerate; the concept cannot carry a tier because authSessionByTokenHash is the pre-actor read that builds the actor, and @serverOnly is unreachable from component/grpc (memql#4769)"

const undeclared4734SessionsForSubjectAdminReason = "memql#4734 -- owner/admin operator read of another person's sessions; the concept cannot carry a tier because authSessionByTokenHash is the pre-actor read that builds the actor, so this query gates itself with requiresOwnerOrAdmin and projects a hash-free shape"

const undeclared4301MagicLinkByIdReason = "memql#4301 -- device-bound flow's by-id read; pre-actor by construction AND the row names an email rather than a user, so no owner field exists to compare; @serverOnly, authorized by the binding cookie in Go"

// undeclared4270InvitationReason covers the console read memql#4270 added when
// user invitations gained an issuing side.
//
// The concept CANNOT declare a tier, and the reason is the one already written
// on v1:identity:invitation and shared with enrolmentToken: the redeem lookup
// (invitationByTokenHash) is PRE-ACTOR. The token IS the credential, so at
// lookup time no authenticated caller exists, an owner tier would compare the
// row against actor.userId == "" and match nothing, and every redeem would turn
// into a silent "invalid". Declaring a tier to satisfy this gate would break the
// flow the gate is protecting.
//
// The read itself is NOT unguarded. pendingUserInvitations carries
// requiresOwnerOrAdmin as a top-level conjunct in its own filter, so the engine
// empties it for a caller below the floor -- the same shape the admin console's
// other reads take, and the reason they were written that way rather than as a
// concept browse.
const undeclared4270InvitationReason = "memql#4270 -- owner/admin console read; the concept cannot carry a tier because its redeem lookup is pre-actor (see undeclared3408EnrolmentReason), and this query gates itself with requiresOwnerOrAdmin instead"

// undeclared4612UserInvitationReason covers the kind="user" half of the redeem
// lookup, split out of invitationByTokenHash by memql#4612.
//
// It is the same read as its guest twin, on the same concept, and it takes the
// same pre-actor argument undeclared4270InvitationReason spells out: the token
// IS the credential, so at lookup time no authenticated caller exists, an owner
// tier would compare the row against actor.userId == "" and match nothing, and
// every redeem would turn into a silent "invalid". Declaring a tier to satisfy
// this gate would break the flow the gate protects.
//
// What memql#4612 changed is which rows come back, not who may ask.
// invitationByTokenHash filters kind=="guest" -- it predates user invitations --
// so the shared store method returned nothing for every kind="user" row and
// user-invitation redemption never once succeeded. The fix is a second door
// rather than a widened one, because widening a credential lookup so a user
// invitation can return through the guest path makes every caller responsible
// for a privilege boundary the filter used to hold.
//
// It carries its own constant rather than the grandfather marker because it is
// NEW. This population only shrinks, and an entry added after the seed has to
// say why it is here rather than inheriting a reason that means "nobody has
// looked at this one yet".
const undeclared4612UserInvitationReason = "memql#4612 -- the kind=\"user\" half of the redeem lookup, split from invitationByTokenHash; pre-actor by construction for the reason undeclared4270InvitationReason records, so a tier on the concept would turn every redeem into a silent invalid"

// undeclared3964RecoveryKeyReason covers the two reads memql#3964 added for the
// owner recovery key.
//
// It is deliberately the SAME argument as undeclared3408EnrolmentReason above,
// because it is the same situation one step further out. An enrolment token
// exists so a person with no credential can obtain their first one; a recovery
// key exists so a cluster OWNER who has lost every sign-in route can obtain
// another. In both cases the presented secret IS the credential and the read
// that validates it runs before any actor exists, so `actor.userId` is the
// empty string at lookup time and an owner tier would compare the row's userId
// against "" and match nothing.
//
// WHAT THAT FAILURE LOOKS LIKE IS WHY IT IS WORTH RESTATING. It is not an
// authorization error anybody would notice: every redemption would simply
// report "invalid", indistinguishable from a typo. A break-glass credential
// that has quietly never worked is only discovered on the one day it is needed,
// so the cost of getting this wrong is paid entirely at the worst moment.
//
// `activeRecoveryKeys` is here for the sibling reason: its caller is the
// identity node's own first-boot mint invariant (memql#3965), running under the
// system actor in the same pre-actor window as activeUsers and
// signInIdentitiesForUser. A self-scoped filter would report every cluster as
// having no recovery key and the invariant would mint a duplicate on every
// boot -- which the advisory lock would then be preventing for the wrong
// reason.
//
// Both ALSO carry @serverOnly, which is a second and narrower statement: no
// client belongs on this surface at all, so neither appears in the generated
// SDK. That does not discharge the tier debt, and the entries are here rather
// than relying on the gate's serverOnly short-circuit so the position is
// stated.
const undeclared3964RecoveryKeyReason = "memql#3964 -- owner break-glass recovery-key reads; the redeem lookup is pre-actor (the key IS the credential) and the mint invariant runs under the system actor at boot, so no owner tier can be compared against"

// undeclared3409SignInRoutesReason covers the one construct memql#3409 added
// for the passkey management surface.
//
// `signInIdentitiesForSelf` filters `userId==actor.userId`, so it is the same
// self-scoped shape as `passkeysForSelf` and `badgesForSelf` and lands here
// for the same single reason the whole list exists: `v1:identity:identity`
// declares no `@rowAuthz` tier.
//
// It is worth naming what the read is FOR, because it is the one read on this
// list whose purpose is to protect the user rather than to serve them: before
// /me/devices revokes a passkey it counts what would remain as a way back in,
// and a read that returned a WIDER set than the caller's own rows would tell
// somebody their account is recoverable on the strength of a stranger's
// credentials. Self-scoping is the correctness property here, not only the
// authorization one.
const undeclared3409SignInRoutesReason = "memql#3409 -- self-scoped sign-in-route count behind the last-credential warning; v1:identity:identity still declares no tier"

// undeclared3591ClaimedOwnerReason covers the one construct memql#3591 added.
//
// `signInIdentitiesForUser` is the sibling of the entry above, for a NAMED user
// rather than the caller, and it lands here for the same single reason the whole
// list exists: `v1:identity:identity` declares no `@rowAuthz` tier and CANNOT
// carry an owner one. That is not a backlog note -- it is measured, in the
// concept's own header (dsl/identity/concepts.memql): `userId` names a
// credential's SUBJECT rather than an ownership claim, four reads are pre-actor by
// construction, and the concept is a union of eight credential kinds with
// different subjects. Declaring a tier is blocked by that, not by this construct.
//
// WHY IT CANNOT BE THE SELF-SCOPED TWIN, which is the question a reader will ask.
// The caller is Store.HasClaimedOwner, running during identity boot to decide
// whether the cluster's owner has ever authenticated. There is no actor in that
// window -- the same circularity that keeps activeUsers and CountActiveUsers
// server-only -- so `userId==actor.userId` would match zero rows and report every
// claimed cluster as unclaimed, which is the answer that re-sends the owner's
// claim email on every deploy.
//
// What bounds it instead: @serverOnly keeps it off the generated SDK entirely, and
// the sole caller stamps internal origin, which the engine enforces against
// auth.CallOrigin.
const undeclared3591ClaimedOwnerReason = "memql#3591 -- pre-actor owner-claim check at identity boot; v1:identity:identity cannot carry a tier (measured, see the concept header)"

// undeclared4208CodeMetricReason covers codeMetricsInWindow, the
// prefix-scoped client read over the continuous-aggregate rollups
// (dsl/observability/queries.memql, memql#4208).
//
// Listed rather than declared, and carrying its own issue rather than the
// grandfather marker. The lock for memql#4208 is "the same read gate
// codeMetric has today": the portal read these rows through the generic
// concept browse, which admits every row of an undeclared, PII-free concept
// to any authenticated caller, and this read is strictly no wider -- it is
// the same rows behind a prefix + bucket + window predicate. Declaring a
// tier on v1:observability:codeMetric is an authorization judgment about
// the observability domain as a whole (codeProfile and invocation declare
// nothing either), and making it as a side effect of a read-shape task is
// the quiet scope change this gate exists to surface.
const undeclared4208CodeMetricReason = "memql#4208 -- prefix-scoped codeMetric read for clients, the same gate as the generic browse it replaces; a tier on v1:observability:codeMetric is the observability domain's decision"

// The reads memql#4369 (Nexus) added, each listed rather than
// declared and each carrying its own blocking issue rather than the
// grandfather marker.
//
// ===========================================================================
// WHY NONE OF THE THREE PAYS ITS DEBT HERE
// ===========================================================================
// Nexus draws ONE GOAL of the caller's -- the plan, its tasks, the agents it
// raised, the artifacts it produced. Each of those three concepts is blocked
// on a DIFFERENT thing, and collapsing them into one reason would hide two of
// the three:
//
//	plan      memql#4366 measured the blocker and it is not a matter of
//	          writing the annotation. Every engine-internal read of `plan`
//	          runs under an actor carrying no AccessContext
//	          (integrations/planner/agent_loop.go's systemActorContext), and
//	          `refuseRowAuthzWithoutActor` refuses such a read on an owned
//	          concept -- so declaring the tier does not narrow those reads,
//	          it BREAKS them. Separately, cmd/memqlmigrate's own inference
//	          already rules that `plan` cannot take an owned floor because
//	          `plansForSpace` reads collaborators' rows BY DESIGN.
//
//	agent     the long tail, and the one where an owner conjunct would be a
//	          SILENT EMPTINESS rather than a gate: `createAgent` takes
//	          `ownerUserId` as a caller ARGUMENT rather than stamping it
//	          from the actor, so a planner-provisioned specialist can carry
//	          an empty one -- and the planner agent itself is owned by no
//	          user at all. agentsForPlan is `@public` for exactly that
//	          reason, narrowed by lineage.originatingPlanId, and every one
//	          of its eight already-listed siblings is here too.
//
// (memql#4369 listed a THIRD, `artifact`, on the same footing. memql#4340
// declared v1:library:artifact's tier -- the composite
// `@rowAuthz(owner="ownerUserId", clusterOwner)` -- so every read over it,
// artifactsForPlan included, is measured now and none belongs on this list.
// That is why only two of the three reasons below survive.)
//
// What Nexus does about the residual is recorded where the long tail is
// tracked rather than only here: docs/public/operate/auth/per-row-authz-audit.md
// carries a table naming these concepts, and states that the client-side
// filter the portal applies closes a deep-link hole and is NOT a gate.
const undeclared4369NexusPlanReason = "memql#4366 -- the caller's own goals, newest first; v1:planner:plan cannot take an owned floor until the engine's internal actor is characterised (measured on that issue), and plansForSpace reads collaborators' rows by design"

const undeclared4369NexusAgentReason = "memql#4369 -- the agents one goal raised, narrowed by lineage.originatingPlanId; v1:agents:agent declares no tier and an owner conjunct here would return an empty set rather than a narrowed one (createAgent takes ownerUserId as a caller arg; the planner agent is owned by no user), so the declaration is #4366's successor work"

// undeclared2803ArtifactLabelReason covers libraryArtifactsByLabel, the
// label-facet read the artifacts-labels feature added over
// v1:library:artifact (dsl/library/queries.memql).
//
// Listed rather than declared, and carrying its own issue rather than the
// grandfather marker. Declaring a tier here is exactly the memql#2803
// decision v1:library:artifact is already waiting on -- createArtifact
// threads ownerUserId from the promoting automation's SOURCE row rather
// than stamping it from actor.userId (promotion runs server-side on the
// owner's behalf, not as the owner; see library_test.go's own note on
// TestEditDocument_OwnerThreadedFromRow), so a tier is a redesign of that
// write path, not a side effect of adding one more read. The read itself
// carries no new risk over FOUR of its five already-listed
// v1:library:artifact siblings immediately above (libraryArtifactById,
// libraryArtifacts, libraryArtifactsByKind, libraryArtifactsByLens): the
// identical ownerUserId==actor.userId top-level conjunct, narrowed by a
// labels membership predicate instead of a lens/kind equality. The fifth,
// libraryWorkspaceLiveSources, is @public with no owner conjunct at all,
// so it is not part of that comparison.
//
// Also covers libraryArtifactBySourceConceptRef, the Go-integration-facing
// read touchArtifact / the label-write capabilities use to resolve the
// CURRENT artifact row by its sourceConceptRef -- same concept, same
// ownerUserId==actor.userId conjunct, same memql#2803 gap, added for the
// same review-round-1 reason (replacing a Go-side re-derivation of
// createArtifact's hash-based id with a declared-field filter).
const undeclared2803ArtifactLabelReason = "memql#2803 -- label facet read; v1:library:artifact still declares no tier (same gap as its five sibling reads), and a tier is a promotion-write-path redesign out of scope for a read-shape addition"

// undeclared3716CORSGrantsReason covers oAuthClientCORSGrants, the read
// memql#3716 added so identity's CORS allowlist stops being a boot-time env
// snapshot.
//
// It postdates the seed and names its own issue rather than carrying the
// grandfather marker, for the same reason the entries above do.
//
// A tier could not simply be declared, and this is a property of the concept
// rather than a deferral. `v1:identity:oauthClient` has NO OWNING USER: a row is
// minted by an unauthenticated stranger at POST /register (RFC 7591 dynamic
// client registration), so there is no field an owner tier could name -- and
// `enforceRowAuthzOnPlan` has no read-side escape, so a wrong tier here fails
// every OAuth flow on the cluster rather than leaking. The other read over this
// concept, `oAuthClientByClientId`, sits on the unauthenticated /oauth/token
// path for the same reason and was grandfathered.
//
// This read is pre-actor in the strongest sense on the list: it runs inside the
// CORS middleware on an OPTIONS PREFLIGHT, which is unauthenticated by
// definition -- the browser sends it before it is willing to send the
// credentialed request. There is no actor.userId in existence at that moment for
// a tier to compare against.
//
// What bounds it instead is its projection: `oauthClientCORSGrant` is the row id,
// the client id and the granted origin list, and nothing else. No redirect URIs,
// no registration metadata. An origin on that list is not a secret -- the CORS
// response header hands it back to the browser that asked -- which is why the
// read stays on the wire while the WRITE that sets it is @serverOnly.
const undeclared3716CORSGrantsReason = "memql#3716 -- the admin-granted CORS allowlist, read on an unauthenticated preflight; v1:identity:oauthClient has no owning user to tier against (rows are minted by strangers at /register)"

// undeclaredEntry is the map's value type, spelled as an alias so the
// literal below reads exactly as memql#3135 specified it while the
// classifier can still name the type in a signature.
//
// #3135 is the whole reason the value is a struct rather than a bare
// concept id. The sibling gate's value IS its tracking reason; the parked
// #3077 attempt repurposed that slot for the concept id, gaining a real
// re-bind check and losing the reason field. Both are load-bearing and
// both are kept here: the concept id catches a construct silently
// re-bound to a different concept, and the reason is what stops an entry
// from being added with no accounting for why.
type undeclaredEntry = struct {
	concept string
	reason  string
}

// undeclaredRowAuthzConstructs pins every query construct whose bound
// concept declares no `@rowAuthz` tier. Shrink-only -- read the header
// before touching it.
var undeclaredRowAuthzConstructs = map[string]struct {
	concept string
	reason  string
}{
	// v1:agents:agent
	"activeAgents":          {"v1:agents:agent", undeclaredGrandfatherReason},
	"activeAgentsForUser":   {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentById":             {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentOwner":            {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentRoleSlugsInUse":   {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentsForPlan":         {"v1:agents:agent", undeclared4369NexusAgentReason},
	"agentsForRegistry":     {"v1:agents:agent", undeclaredGrandfatherReason},
	"allAgents":             {"v1:agents:agent", undeclaredGrandfatherReason},
	"assistantAgentForUser": {"v1:agents:agent", undeclaredGrandfatherReason},

	// v1:agents:agentAuthorization -- DEBT PAID, entry DELETED (memql#3177).
	// The seed entry here was `agentAuthorizationsForUser`. The concept now
	// declares `@rowAuthz(owner="userId")` and the construct is
	// `agentAuthorizationsForSelf` filtering `userId==actor.userId`, so it is
	// MEASURED rather than unmeasured and this file's own rule applies: a
	// stale entry silently suppresses that construct forever. This comment is
	// not an entry -- the map is one shorter than it was, which is the only
	// direction it may move.

	// v1:agents:agentRole
	"activeAgentRoles": {"v1:agents:agentRole", undeclaredGrandfatherReason},
	"agentRoleBySlug":  {"v1:agents:agentRole", undeclaredGrandfatherReason},

	// v1:agents:avatarPersona
	"avatarPersonaById": {"v1:agents:avatarPersona", undeclaredGrandfatherReason},
	"avatarPersonas":    {"v1:agents:avatarPersona", undeclaredGrandfatherReason},

	// v1:agents:skill
	"activeSkills":      {"v1:agents:skill", undeclaredGrandfatherReason},
	"activeSkillsFull":  {"v1:agents:skill", undeclaredGrandfatherReason},
	"skillBySlug":       {"v1:agents:skill", undeclaredGrandfatherReason},
	"skillNeedsRefresh": {"v1:agents:skill", undeclaredGrandfatherReason},

	// v1:agents:skillChangeEvent
	"skillChangeEventsForAgent": {"v1:agents:skillChangeEvent", undeclaredGrandfatherReason},

	// v1:authoring:bundle
	"activeAuthoringBundles":           {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundleById":              {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundleForPlan":           {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundleForResponsibility": {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundlesForOwner":         {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"systemActiveAuthoringBundles":     {"v1:authoring:bundle", undeclaredGrandfatherReason},

	// v1:cluster:cluster
	"existingCluster": {"v1:cluster:cluster", undeclaredGrandfatherReason},

	// v1:cluster:deployment
	"deploymentById":        {"v1:cluster:deployment", undeclaredGrandfatherReason},
	"deploymentsForCluster": {"v1:cluster:deployment", undeclaredGrandfatherReason},
	"supersededDeployments": {"v1:cluster:deployment", undeclaredGrandfatherReason},

	// v1:cluster:deploymentNodeSpec
	"nodeSpecsForDeployment": {"v1:cluster:deploymentNodeSpec", undeclaredGrandfatherReason},

	// v1:cluster:node
	"clusterNodes":         {"v1:cluster:node", undeclaredGrandfatherReason},
	"nodesForDeployment":   {"v1:cluster:node", undeclaredGrandfatherReason},
	"nodesNotInDeployment": {"v1:cluster:node", undeclaredGrandfatherReason},
	"staleClusterNodes":    {"v1:cluster:node", undeclaredGrandfatherReason},

	// v1:cluster:nodeType
	"clusterNodeTypes": {"v1:cluster:nodeType", undeclaredGrandfatherReason},

	// v1:cluster:spawnEvent
	"clusterSpawnEvents": {"v1:cluster:spawnEvent", undeclaredGrandfatherReason},

	// v1:cognition:audioOverride
	"audioOverridesForSpace": {"v1:cognition:audioOverride", undeclaredGrandfatherReason},

	// v1:cognition:participant
	"activeHumanParticipants": {"v1:cognition:participant", undeclaredGrandfatherReason},
	"groupGAForSpace":         {"v1:cognition:participant", undeclaredGrandfatherReason},
	"participantByAgentSpace": {"v1:cognition:participant", undeclaredGrandfatherReason},
	"siParticipantForSpace":   {"v1:cognition:participant", undeclaredGrandfatherReason},
	"spaceParticipants":       {"v1:cognition:participant", undeclaredGrandfatherReason},

	// v1:cognition:participant:presence
	"spaceParticipantPresence": {"v1:cognition:participant:presence", undeclaredGrandfatherReason},

	// v1:cognition:session
	"participantSession": {"v1:cognition:session", undeclaredGrandfatherReason},

	// v1:cognition:utterance
	"agentInteractionCount":       {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"feedbackAnnouncementForPlan": {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"greetingUtterance":           {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"hasAIResponseForReply":       {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"spaceUtterances":             {"v1:cognition:utterance", undeclaredGrandfatherReason},

	// v1:cognition:videoOverride
	"videoOverridesForSpace": {"v1:cognition:videoOverride", undeclaredGrandfatherReason},

	// v1:common:attachment
	"attachmentById": {"v1:common:attachment", undeclaredGrandfatherReason},

	// v1:common:media
	"spaceMedia": {"v1:common:media", undeclaredGrandfatherReason},

	// v1:data:log
	"validationLog": {"v1:data:log", undeclaredGrandfatherReason},

	// v1:data:policy
	"policy": {"v1:data:policy", undeclaredGrandfatherReason},

	// v1:data:record
	"detectConflicts": {"v1:data:record", undeclaredGrandfatherReason},
	"recordsByState":  {"v1:data:record", undeclaredGrandfatherReason},
	"usableRecords":   {"v1:data:record", undeclaredGrandfatherReason},

	// v1:forge:project
	"activeProjects": {"v1:forge:project", undeclaredGrandfatherReason},
	"projectById":    {"v1:forge:project", undeclaredGrandfatherReason},
	"projectBySlug":  {"v1:forge:project", undeclaredGrandfatherReason},

	// v1:forge:request
	"approvalQueue":   {"v1:forge:request", undeclaredGrandfatherReason},
	"myRequests":      {"v1:forge:request", undeclaredGrandfatherReason},
	"projectRequests": {"v1:forge:request", undeclaredGrandfatherReason},
	"requestById":     {"v1:forge:request", undeclaredGrandfatherReason},
	"validationQueue": {"v1:forge:request", undeclaredGrandfatherReason},

	// v1:forge:requestEvent
	"requestEvents": {"v1:forge:requestEvent", undeclaredGrandfatherReason},

	// v1:identity:accessRequest
	"accessRequestById":            {"v1:identity:accessRequest", undeclaredGrandfatherReason},
	"expiredPendingAccessRequests": {"v1:identity:accessRequest", undeclaredGrandfatherReason},
	"pendingAccessRequests":        {"v1:identity:accessRequest", undeclaredGrandfatherReason},

	// v1:identity:accountEntitlement
	"accountEntitlement": {"v1:identity:accountEntitlement", undeclaredGrandfatherReason},

	// v1:identity:auditEvent's four entries are gone: the concept declares
	// @rowAuthz(clusterOwner) (memql#4366). The RULING is on the concept, and it
	// is worth knowing before reading the four reads: the cluster-wide security
	// log is OWNER-ONLY, because neither an owner tier nor the composite could
	// express "an admin reads all" -- clusterOwner is Role==owner alone, and
	// createAuditEvent writes actorUserId from caller args (correctly: it is
	// empty for an anonymous event).

	// v1:identity:authCode
	"authCodeByCodeHash":       {"v1:identity:authCode", undeclaredGrandfatherReason},
	"expiredConsumedAuthCodes": {"v1:identity:authCode", undeclaredGrandfatherReason},

	// v1:identity:authSession
	"authSessionByPreviousRefreshTokenHash": {"v1:identity:authSession", undeclaredGrandfatherReason},
	"authSessionByRefreshTokenHash":         {"v1:identity:authSession", undeclaredGrandfatherReason},
	"authSessionByTokenHash":                {"v1:identity:authSession", undeclaredGrandfatherReason},
	"authSessionsForSelfIncludingRevoked":   {"v1:identity:authSession", undeclared4768OwnSessionsReason},
	"authSessionsForSelf":                   {"v1:identity:authSession", undeclared4306SelfSessionsReason},
	"sessionsForSubjectAdmin":               {"v1:identity:authSession", undeclared4734SessionsForSubjectAdminReason},

	// v1:identity:clusterSettings
	"clusterSettingsCurrent": {"v1:identity:clusterSettings", undeclaredGrandfatherReason},

	// v1:identity:deviceCode
	"deviceCodeByDeviceCodeHash": {"v1:identity:deviceCode", undeclared3410DeviceCodeReason},
	"deviceCodeByUserCodeHash":   {"v1:identity:deviceCode", undeclared3410DeviceCodeReason},

	// v1:identity:delegation
	"activeDelegationsByIdentitySubject": {"v1:identity:delegation", undeclaredGrandfatherReason},
	"activeDelegationsForAgent":          {"v1:identity:delegation", undeclaredGrandfatherReason},
	"delegationsByIdentity":              {"v1:identity:delegation", undeclaredGrandfatherReason},
	"expiredActiveDelegations":           {"v1:identity:delegation", undeclaredGrandfatherReason},

	// v1:identity:identity
	"accountTokenById":            {"v1:identity:identity", undeclared3322AccountTokenReason},
	"accountTokensForAccount":     {"v1:identity:identity", undeclared3322AccountTokenReason},
	"badgeByKeyHash":              {"v1:identity:identity", undeclaredGrandfatherReason},
	"badgesForSelf":               {"v1:identity:identity", undeclared3178SelfScopedReason},
	"nodeTokenIdentities":         {"v1:identity:identity", undeclaredGrandfatherReason},
	"nodeTokenIdentitiesAdmin":    {"v1:identity:identity", undeclared3324NodeTokenAdminReason},
	"nodeTokenIdentityByBinding":  {"v1:identity:identity", undeclaredGrandfatherReason},
	"nodeTokenIdentityById":       {"v1:identity:identity", undeclaredGrandfatherReason},
	"voiceAgentTokenIdentityById": {"v1:identity:identity", "memql#4111: read by the voice-agent revocation gate on the auth path, before any actor exists. v1:identity:identity cannot carry an owner tier (see identity_credential_rowauthz_inventory_3349_test.go); classified machine-credential there."},
	"patIdentitiesForSelf":        {"v1:identity:identity", undeclared3178SelfScopedReason},
	"patIdentitiesForUser":        {"v1:identity:identity", undeclaredGrandfatherReason},
	"patIdentityById":             {"v1:identity:identity", undeclaredGrandfatherReason},
	"oidcIdentityBySubject":       {"v1:identity:identity", undeclared4611OidcReason},
	"passkeyByCredentialId":       {"v1:identity:identity", undeclared3406PasskeyReason},
	"passkeysForSelf":             {"v1:identity:identity", undeclared3406PasskeyReason},
	"recoveryKeyByHash":           {"v1:identity:identity", undeclared3964RecoveryKeyReason},
	"activeRecoveryKeys":          {"v1:identity:identity", undeclared3964RecoveryKeyReason},
	"signInIdentitiesForSelf":     {"v1:identity:identity", undeclared3409SignInRoutesReason},
	"signInIdentitiesForUser":     {"v1:identity:identity", undeclared3591ClaimedOwnerReason},
	"patIdentityByKeyHash":        {"v1:identity:identity", undeclaredGrandfatherReason},
	"workerTokenByKeyHash":        {"v1:identity:identity", undeclaredGrandfatherReason},
	"workerTokensForUser":         {"v1:identity:identity", undeclaredGrandfatherReason},

	// v1:identity:enrolmentToken
	"enrolmentTokenByHash": {"v1:identity:enrolmentToken", undeclared3408EnrolmentReason},

	// v1:identity:invitation
	"invitationById":                {"v1:identity:invitation", undeclaredGrandfatherReason},
	"invitationByPreviousTokenHash": {"v1:identity:invitation", undeclaredGrandfatherReason},
	"invitationByTokenHash":         {"v1:identity:invitation", undeclaredGrandfatherReason},
	"pendingUserInvitations":        {"v1:identity:invitation", undeclared4270InvitationReason},
	"userInvitationByTokenHash":     {"v1:identity:invitation", undeclared4612UserInvitationReason},

	// v1:identity:magicLinkRequest
	"expiredMagicLinkRequests":    {"v1:identity:magicLinkRequest", undeclaredGrandfatherReason},
	"magicLinkRequestByTokenHash": {"v1:identity:magicLinkRequest", undeclaredGrandfatherReason},
	"magicLinkRequestById":        {"v1:identity:magicLinkRequest", undeclared4301MagicLinkByIdReason},

	// v1:identity:oauthClient
	"oAuthClientByClientId": {"v1:identity:oauthClient", undeclaredGrandfatherReason},
	"oAuthClientCORSGrants": {"v1:identity:oauthClient", undeclared3716CORSGrantsReason},

	// v1:identity:user
	"activeUsers":               {"v1:identity:user", undeclaredGrandfatherReason},
	"currentUser":               {"v1:identity:user", undeclaredGrandfatherReason},
	"searchUsers":               {"v1:identity:user", undeclaredGrandfatherReason},
	"userActiveSpace":           {"v1:identity:user", undeclaredGrandfatherReason},
	"userByEmail":               {"v1:identity:user", undeclaredGrandfatherReason},
	"userById":                  {"v1:identity:user", undeclaredGrandfatherReason},
	"userByIdSystem":            {"v1:identity:user", undeclaredGrandfatherReason},
	"userCount":                 {"v1:identity:user", undeclaredGrandfatherReason},
	"userDisplayById":           {"v1:identity:user", undeclaredGrandfatherReason},
	"usersActiveInSpace":        {"v1:identity:user", undeclaredGrandfatherReason},
	"usersForSeedSweep":         {"v1:identity:user", undeclared3217SeedSweepReason},
	"usersInDeletionCooldown":   {"v1:identity:user", undeclaredGrandfatherReason},
	"usersScheduledForDeletion": {"v1:identity:user", undeclaredGrandfatherReason},

	// v1:identity:workerPairingCode
	"workerPairingCodeByHash": {"v1:identity:workerPairingCode", undeclaredGrandfatherReason},

	// v1:knowledge:documentChunk
	"allDocumentChunkDomains": {"v1:knowledge:documentChunk", undeclaredGrandfatherReason},
	"documentChunksForDomain": {"v1:knowledge:documentChunk", undeclaredGrandfatherReason},

	// v1:observability:codeMetric
	"codeMetricsInWindow": {"v1:observability:codeMetric", undeclared4208CodeMetricReason},

	// v1:planner:plan
	"activePlansForUser":               {"v1:planner:plan", undeclaredGrandfatherReason},
	"allPlans":                         {"v1:planner:plan", undeclaredGrandfatherReason},
	"awaitingFeedbackPlansPastTimeout": {"v1:planner:plan", undeclaredGrandfatherReason},
	"historicalPlanMetrics":            {"v1:planner:plan", undeclaredGrandfatherReason},
	"planById":                         {"v1:planner:plan", undeclaredGrandfatherReason},
	"plansForResponsibility":           {"v1:planner:plan", undeclaredGrandfatherReason},
	"plansForSpace":                    {"v1:planner:plan", undeclaredGrandfatherReason},
	"plansForUser":                     {"v1:planner:plan", undeclared4369NexusPlanReason},
	"runningPlansForUser":              {"v1:planner:plan", undeclaredGrandfatherReason},
	"strandedCandidatePlans":           {"v1:planner:plan", undeclaredGrandfatherReason},
	"waitingPlansForUser":              {"v1:planner:plan", undeclaredGrandfatherReason},

	// v1:planner:responsibility
	"activeResponsibilities":            {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"activeResponsibilitiesAcrossUsers": {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"dueResponsibilities":               {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"responsibilitiesForUser":           {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"responsibilityById":                {"v1:planner:responsibility", undeclaredGrandfatherReason},

	// v1:planner:task
	"tasksForPlan": {"v1:planner:task", undeclaredGrandfatherReason},

	// v1:planner:taskState
	"taskStateById": {"v1:planner:taskState", undeclaredGrandfatherReason},

	// v1:platform:globalVariable
	"globalVariable":  {"v1:platform:globalVariable", undeclaredGrandfatherReason},
	"globalVariables": {"v1:platform:globalVariable", undeclaredGrandfatherReason},
	"userDefaults":    {"v1:platform:globalVariable", undeclaredGrandfatherReason},

	// v1:platform:inboundRequest
	"inboundRequestByDedupeKey": {"v1:platform:inboundRequest", undeclaredGrandfatherReason},
	"inboundRequestById":        {"v1:platform:inboundRequest", undeclared3461ByIdReason},
	"inboundRequestsByStatus":   {"v1:platform:inboundRequest", undeclaredGrandfatherReason},

	// v1:platform:missingCapability
	"missingCapabilitiesByStatus":    {"v1:platform:missingCapability", undeclaredGrandfatherReason},
	"missingCapabilityByKindAndName": {"v1:platform:missingCapability", undeclaredGrandfatherReason},

	// v1:platform:outboundRequest
	"outboundRequestsByStatus": {"v1:platform:outboundRequest", undeclaredGrandfatherReason},

	// v1:rbac:capability
	"activeCapabilities":          {"v1:rbac:capability", undeclaredGrandfatherReason},
	"capabilitiesForResourceType": {"v1:rbac:capability", undeclaredGrandfatherReason},
	"capabilitiesForRole":         {"v1:rbac:capability", undeclaredGrandfatherReason},
	"capabilityGrant":             {"v1:rbac:capability", undeclaredGrandfatherReason},

	// v1:rbac:role
	"activeRoles": {"v1:rbac:role", undeclaredGrandfatherReason},
	"roleBySlug":  {"v1:rbac:role", undeclaredGrandfatherReason},

	// v1:router:budget
	"routerBudgets": {"v1:router:budget", undeclaredGrandfatherReason},

	// v1:safety:approvalRequest
	"activeApprovalsByCorrelationKey": {"v1:safety:approvalRequest", undeclaredGrandfatherReason},
	"approvalRequestById":             {"v1:safety:approvalRequest", undeclaredGrandfatherReason},

	// v1:safety:classification
	"allSafetyClassifications": {"v1:safety:classification", undeclaredGrandfatherReason},

	// v1:safety:outputScreening
	"allOutputScreenings": {"v1:safety:outputScreening", undeclaredGrandfatherReason},

	// v1:telephony:consent
	"consentOptOut": {"v1:telephony:consent", undeclaredGrandfatherReason},

	// v1:telephony:number
	"allNumbers":         {"v1:telephony:number", undeclaredGrandfatherReason},
	"numberByE164":       {"v1:telephony:number", undeclaredGrandfatherReason},
	"numbersByPartition": {"v1:telephony:number", undeclaredGrandfatherReason},

	// v1:workbench:workspace and v1:worker:registration PAID OFF in epic
	// memql#4349: both concepts now declare
	// @rowAuthz(owner="ownerUserId", clusterOwner), so their four reads
	// (provisionedWorkspaces, workspaceForPlan, workerByIdentityId,
	// workersForUser) are measured rather than unmeasured and their entries are
	// deleted. Each gained the caller conjunct at the same time, which is what
	// kept the enforcement gate green -- an entry can only leave this list by
	// the concept declaring, never by the read moving.

	// v1:worker:invocation had FIVE entries here -- three grandfathered and the
	// two Fleet reads added by epic memql#4349 -- and they are gone because the
	// concept now declares @rowAuthz(owner="ownerUserId", clusterOwner)
	// (memql#4406).
	//
	// The blocker those entries recorded was real and is worth keeping written
	// down, because it is the shape every remaining sweep-adjacent entry below
	// has: `contextWithSystemActor` stamps RoleReader, so declaring the tier
	// made the retention sweep read zero rows and retire nothing -- silently,
	// with WORKER_INVOCATION_RETENTION_DAYS quietly ceasing to mean anything
	// and every gate in this file green.
	//
	// It was resolved by DECIDING the sweep's principal rather than discovering
	// it: workerInvocationRetentionSweep is on the engine-owned maintenance list
	// (component/automations/maintenance_actor.go) and runs as a named synthetic
	// cluster owner, which the composite tier admits. An identity, not an
	// enforcement bypass -- component/campaigns/worker.go's argument.
}

// undeclaredWorld is the state the pinned list is judged against: what
// the LOADED tree says today.
type undeclaredWorld struct {
	// queryConcept maps every query construct in the registry to the
	// concept its signature binds ("" when it binds none). A construct
	// ABSENT from this map is not a query in the tree any more.
	queryConcept map[string]string
	// conceptLoaded reports whether a concept id resolves in the loaded
	// concept registry at all.
	//
	// Kept separate from conceptDeclares on purpose. Folding "the concept
	// is gone" into "the concept does not declare a tier" is the exact
	// defect this gate was rebuilt to avoid -- see classifyUndeclared.
	conceptLoaded map[string]bool
	// conceptDeclares reports whether a LOADED concept carries a tier.
	conceptDeclares map[string]bool
}

// deriveUndeclaredWorld reads the world off the loaded registries.
func deriveUndeclaredWorld(registry *FunctionRegistry) undeclaredWorld {
	w := undeclaredWorld{
		queryConcept:    map[string]string{},
		conceptLoaded:   map[string]bool{},
		conceptDeclares: map[string]bool{},
	}
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" {
			continue
		}
		concept := strings.TrimSpace(fn.BoundConcept)
		w.queryConcept[fn.Name] = concept
		if concept == "" {
			continue
		}
		c, err := memoryNodes.Get(concept)
		if err != nil || c == nil {
			continue
		}
		w.conceptLoaded[concept] = true
		w.conceptDeclares[concept] = c.RowAuthz != nil
	}
	return w
}

// undeclaredDebt is the population the list pins: query constructs bound
// to a loaded concept that declares no tier, as construct -> concept.
func undeclaredDebt(w undeclaredWorld) map[string]string {
	debt := map[string]string{}
	for name, concept := range w.queryConcept {
		if concept == "" || !w.conceptLoaded[concept] || w.conceptDeclares[concept] {
			continue
		}
		debt[name] = concept
	}
	return debt
}

// undeclaredFindings is one classification pass. Every field is a list of
// human-readable lines; empty everywhere means the list and the tree
// agree exactly.
type undeclaredFindings struct {
	// newDebt: derived, not listed. The list must grow to cover it, or --
	// preferably -- the concept must declare a tier.
	newDebt []string
	// debtPaid: listed, and the concept now declares a tier. Delete it.
	debtPaid []string
	// gone: listed, and the construct no longer exists. Delete it.
	gone []string
	// unresolvable: listed, the construct still exists, and its concept
	// could not be resolved at all. NOT the same as debt paid -- see
	// classifyUndeclared.
	unresolvable []string
	// rebound: listed against one concept, now bound to another.
	rebound []string
	// missingReason: listed with an empty reason (memql#3135).
	missingReason []string
}

func (f undeclaredFindings) empty() bool {
	return len(f.newDebt) == 0 && len(f.debtPaid) == 0 && len(f.gone) == 0 &&
		len(f.unresolvable) == 0 && len(f.rebound) == 0 && len(f.missingReason) == 0
}

// classifyUndeclared compares the pinned list against the derived world.
//
// # THE CLASSIFICATION THAT MUST NOT BE MADE
//
// A pinned entry that is no longer in the derived debt set has stopped
// being debt for one of several reasons, and they are NOT interchangeable:
//
//   - the concept declares a tier now -- debt paid, delete the entry;
//   - the construct is gone -- delete the entry;
//   - the construct is still there but its concept does not RESOLVE.
//
// The third is not "debt paid" and the entry must not be deleted for it.
// An earlier build of this gate treated any entry absent from the derived
// set as paid, so a single concept failing to resolve would have reported
// its constructs as debt paid and, taken at face value, emptied the whole
// ratchet in one commit -- the list is the only record of the population,
// so deleting it on a bad signal is unrecoverable by inspection.
//
// In the ordinary case a dropped concept takes its queries down with it
// and the run never reaches here: the loader skips those constructs and
// the gate's skip guard fatals first. This arm covers the case where it
// does not, and it exists because the cost of the two errors is wildly
// asymmetric -- an entry wrongly kept costs one investigation, an entry
// wrongly deleted costs the ratchet.
func classifyUndeclared(pinned map[string]undeclaredEntry, w undeclaredWorld) undeclaredFindings {
	var f undeclaredFindings
	debt := undeclaredDebt(w)

	for name, concept := range debt {
		if _, listed := pinned[name]; !listed {
			f.newDebt = append(f.newDebt, fmt.Sprintf("%s (%s)", name, concept))
		}
	}

	for name, entry := range pinned {
		if strings.TrimSpace(entry.reason) == "" {
			f.missingReason = append(f.missingReason, fmt.Sprintf("%s (%s)", name, entry.concept))
		}

		concept, isQuery := w.queryConcept[name]
		switch {
		case !isQuery:
			f.gone = append(f.gone, fmt.Sprintf(
				"%s -- listed against %s, but no query construct by that name is in the tree", name, entry.concept))
		case concept == "":
			f.unresolvable = append(f.unresolvable, fmt.Sprintf(
				"%s -- still a query, but it now binds no concept at all (listed against %s)", name, entry.concept))
		case !w.conceptLoaded[concept]:
			f.unresolvable = append(f.unresolvable, fmt.Sprintf(
				"%s -- still a query bound to %s, but that concept does not resolve in the loaded tree", name, concept))
		case concept != entry.concept:
			f.rebound = append(f.rebound, fmt.Sprintf(
				"%s -- listed against %s, now bound to %s", name, entry.concept, concept))
		case w.conceptDeclares[concept]:
			f.debtPaid = append(f.debtPaid, fmt.Sprintf(
				"%s -- %s declares a tier now", name, concept))
		}
	}

	for _, l := range [][]string{f.newDebt, f.debtPaid, f.gone, f.unresolvable, f.rebound, f.missingReason} {
		sort.Strings(l)
	}
	return f
}

// TestUndeclaredRowAuthzPopulationOnlyShrinks is the ratchet.
func TestUndeclaredRowAuthzPopulationOnlyShrinks(t *testing.T) {
	// Load SKIPS are load-bearing, exactly as they are for the sibling
	// owner gate (memql#2909): a construct that failed to parse is a
	// construct this gate cannot see, and silence must not read as a
	// pass. Worse here than there -- a skipped construct would be
	// classified as "gone" and its entry deleted, converting a parse
	// error into a permanent hole in the ratchet.
	conceptCount, conceptSkips, err := LoadUnifiedConceptsWithSkips(nil)
	if err != nil {
		t.Fatalf("LoadUnifiedConceptsWithSkips: %v", err)
	}
	if len(conceptSkips) > 0 {
		t.Fatalf("%d concept(s) failed to load, so this gate cannot see them: %v",
			len(conceptSkips), conceptSkips)
	}
	if conceptCount == 0 {
		t.Fatal("no concepts loaded")
	}

	registry := newFunctionRegistry()
	report := newLoadReport()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), report); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("%d construct(s) were skipped at load, so this gate cannot see them and would "+
			"report their pinned entries as gone:\n  %v", len(report.Skipped), report.Skipped)
	}

	world := deriveUndeclaredWorld(registry)
	if len(world.queryConcept) == 0 {
		t.Fatal("no query constructs loaded -- the derivation saw nothing, so every pinned entry " +
			"would classify as gone. That is a broken run, not a paid-off ratchet.")
	}
	if len(undeclaredDebt(world)) == 0 && len(undeclaredRowAuthzConstructs) > 0 {
		t.Fatal("the derived undeclared set is EMPTY while the pinned list is not. Either every " +
			"concept in the tree now declares a tier -- in which case this whole file should be " +
			"deleted in a change that says so -- or the derivation broke. Do not empty the list " +
			"on this signal alone; it is the only record of the population.")
	}

	f := classifyUndeclared(undeclaredRowAuthzConstructs, world)

	if len(f.newDebt) > 0 {
		t.Errorf(`%d query construct(s) read a concept that declares no @rowAuthz tier and are not on the list:

  %s

This list only shrinks. A construct here is a new read over an unmeasured concept -- the
population memql#2920's tier was introduced to close, on the surface where the credential
concepts live.

Two fixes, and they are different decisions:
  - Declare a tier on the concept (@rowAuthz(...)) -- the one that actually pays the debt,
    and the reason this gate exists.
  - Add the construct here WITH ITS OWN FILED ISSUE as the reason, if a declaration is
    genuinely blocked. The seed entries share a grandfather marker precisely so a new
    entry carrying an issue number stands out from them.`,
			len(f.newDebt), strings.Join(f.newDebt, "\n  "))
	}

	if len(f.debtPaid) > 0 {
		t.Errorf(`%d entry(ies) name a concept that DECLARES a tier now:

  %s

Delete them. The debt is paid, and a stale entry is as bad as a missing gate -- it
suppresses that construct for whoever writes the next query over it.`,
			len(f.debtPaid), strings.Join(f.debtPaid, "\n  "))
	}

	if len(f.gone) > 0 {
		t.Errorf(`%d entry(ies) name a construct that is no longer in the tree:

  %s

Delete them.`, len(f.gone), strings.Join(f.gone, "\n  "))
	}

	if len(f.unresolvable) > 0 {
		t.Errorf(`%d entry(ies) name a construct whose bound concept does not resolve:

  %s

This is NOT "debt paid" and these entries must not be deleted on this signal. A concept
that fails to resolve makes its constructs unjudgeable, not safe. Find out why the concept
is missing first -- an entry wrongly kept costs one investigation, an entry wrongly deleted
costs the ratchet's only record of the population.`,
			len(f.unresolvable), strings.Join(f.unresolvable, "\n  "))
	}

	if len(f.rebound) > 0 {
		t.Errorf(`%d entry(ies) are listed against one concept but bound to another:

  %s

The construct moved. Re-point the entry only after checking the new concept -- a construct
re-bound to a different undeclared concept is a different authorization question from the
one that was grandfathered.`, len(f.rebound), strings.Join(f.rebound, "\n  "))
	}

	if len(f.missingReason) > 0 {
		t.Errorf(`%d entry(ies) carry no reason:

  %s

Every entry accounts for itself (memql#3135): the shared grandfather marker for the seed,
a filed issue number for anything added since. An entry with no reason is how a ratchet
turns into decoration.`, len(f.missingReason), strings.Join(f.missingReason, "\n  "))
	}

	if f.empty() {
		t.Logf("%d pinned construct(s) over %d undeclared concept(s); list and tree agree exactly",
			len(undeclaredRowAuthzConstructs), countUndeclaredConcepts(world))
	}
}

// countUndeclaredConcepts reports how many distinct concepts the pinned
// population spans, for the pass-case log line.
func countUndeclaredConcepts(w undeclaredWorld) int {
	seen := map[string]bool{}
	for _, concept := range undeclaredDebt(w) {
		seen[concept] = true
	}
	return len(seen)
}

// ---- classifier unit tests ----
//
// These run the classifier against a synthetic world rather than the
// tree, so each direction of the gate is proven to bite without anyone
// having to edit .memql files to see it fail.

// worldOf builds a synthetic world. concepts maps a concept id to whether
// it declares a tier; a concept id absent from it does not resolve.
func worldOf(queries map[string]string, concepts map[string]bool) undeclaredWorld {
	w := undeclaredWorld{
		queryConcept:    map[string]string{},
		conceptLoaded:   map[string]bool{},
		conceptDeclares: map[string]bool{},
	}
	for name, concept := range queries {
		w.queryConcept[name] = concept
	}
	for concept, declares := range concepts {
		w.conceptLoaded[concept] = true
		w.conceptDeclares[concept] = declares
	}
	return w
}

func TestUndeclaredGateCatchesNewDebt(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{"listed": {"v1:x:thing", "seed"}},
		worldOf(
			map[string]string{"listed": "v1:x:thing", "brandNew": "v1:x:thing"},
			map[string]bool{"v1:x:thing": false},
		),
	)
	if len(f.newDebt) != 1 || !strings.Contains(f.newDebt[0], "brandNew") {
		t.Fatalf("a new query over an undeclared concept must be reported as new debt, got %+v", f)
	}
	if len(f.debtPaid) != 0 || len(f.gone) != 0 {
		t.Fatalf("the listed construct is still debt and must produce no staleness finding, got %+v", f)
	}
}

func TestUndeclaredGateCatchesStaleEntries(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{
			"nowDeclared": {"v1:x:declared", "seed"},
			"deleted":     {"v1:x:thing", "seed"},
			"stillDebt":   {"v1:x:thing", "seed"},
		},
		worldOf(
			map[string]string{"nowDeclared": "v1:x:declared", "stillDebt": "v1:x:thing"},
			map[string]bool{"v1:x:declared": true, "v1:x:thing": false},
		),
	)
	if len(f.debtPaid) != 1 || !strings.Contains(f.debtPaid[0], "nowDeclared") {
		t.Errorf("an entry whose concept now declares a tier must be reported as debt paid, got %+v", f)
	}
	if len(f.gone) != 1 || !strings.Contains(f.gone[0], "deleted") {
		t.Errorf("an entry whose construct is gone must be reported as gone, got %+v", f)
	}
	// The two reasons are reported separately because they are different
	// facts about the tree, which is what lets the failure message name
	// which one applies.
	if strings.Contains(strings.Join(f.gone, " "), "nowDeclared") ||
		strings.Contains(strings.Join(f.debtPaid, " "), "deleted") {
		t.Errorf("the two staleness reasons must not be conflated, got %+v", f)
	}
}

// TestUndeclaredGateDoesNotClassifyMissingConceptAsDebtPaid pins the
// defect described on classifyUndeclared: an unresolvable concept read as
// "debt paid" would invite deleting the whole ratchet in one commit.
func TestUndeclaredGateDoesNotClassifyMissingConceptAsDebtPaid(t *testing.T) {
	pinned := map[string]undeclaredEntry{
		"orphanA": {"v1:x:vanished", "seed"},
		"orphanB": {"v1:x:vanished", "seed"},
	}
	// The constructs still load; their concept does not resolve.
	f := classifyUndeclared(pinned, worldOf(
		map[string]string{"orphanA": "v1:x:vanished", "orphanB": "v1:x:vanished"},
		map[string]bool{"v1:x:other": false},
	))

	if len(f.debtPaid) != 0 {
		t.Fatalf("a MISSING concept classified as debt paid: %v\nTaken at face value that empties "+
			"the ratchet -- the entries would be deleted for a fact that was never established.", f.debtPaid)
	}
	if len(f.gone) != 0 {
		t.Fatalf("the constructs still exist, so 'gone' is also the wrong answer: %v", f.gone)
	}
	if len(f.unresolvable) != 2 {
		t.Fatalf("both entries must be reported as unresolvable, got %+v", f)
	}
	for _, line := range f.unresolvable {
		if !strings.Contains(line, "does not resolve") {
			t.Errorf("the finding must say the concept did not resolve, got %q", line)
		}
	}
}

func TestUndeclaredGateCatchesRebind(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{"moved": {"v1:x:thing", "seed"}},
		worldOf(
			map[string]string{"moved": "v1:x:other"},
			map[string]bool{"v1:x:thing": false, "v1:x:other": false},
		),
	)
	if len(f.rebound) != 1 || !strings.Contains(f.rebound[0], "v1:x:other") {
		t.Fatalf("a construct re-bound to another concept must be reported, got %+v", f)
	}
	// Reported ONCE, as a re-bind. The construct is listed by name, so it
	// is not unlisted debt; saying both would read as two problems when it
	// is one, and the re-bind line is the one that names what changed.
	if len(f.newDebt) != 0 {
		t.Fatalf("a re-bind is one finding, not two, got %+v", f)
	}
}

func TestUndeclaredGateCatchesMissingReason(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{
			"noReason":    {"v1:x:thing", ""},
			"blankReason": {"v1:x:thing", "   "},
			"ok":          {"v1:x:thing", undeclaredGrandfatherReason},
		},
		worldOf(
			map[string]string{"noReason": "v1:x:thing", "blankReason": "v1:x:thing", "ok": "v1:x:thing"},
			map[string]bool{"v1:x:thing": false},
		),
	)
	if len(f.missingReason) != 2 {
		t.Fatalf("an entry added without a reason must fail the gate, got %+v", f.missingReason)
	}
	if strings.Contains(strings.Join(f.missingReason, " "), "\"ok\"") {
		t.Fatalf("the entry carrying a reason must not be flagged, got %+v", f.missingReason)
	}
}
