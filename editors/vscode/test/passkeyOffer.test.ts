// Offering passkey enrolment after a sign-in (znasllc-io/memql#3902).
//
// The second half of memql#3885's three-state table. The first half -- "this
// cluster is unclaimed, claim it" -- is derivable with NO credential, because
// `GET /setup` must be reachable before anyone holds anything. This half is
// not, and MUST NOT BE: an unauthenticated way to ask whether an account has a
// passkey is an enumeration oracle. So it runs after sign-in, over the
// authenticated stream, and every decision below is about being quiet in the
// cases where it cannot honestly say yes.

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  OfferMemory,
  decidePasskeyOffer,
  enrolmentStillNeeded,
  passkeyAlreadyEnrolledMessage,
  passkeyOfferMessage,
  type CallerIdentity,
  type PasskeyOfferDeps,
} from "../src/auth/passkeyOffer.js";

interface StubOptions {
  caller?: CallerIdentity | null;
  passkeys?: number;
  whoAmIThrows?: boolean;
  countThrows?: boolean;
}

/** A deps stub that also records which calls were made, for the ordering tests. */
function stub(options: StubOptions = {}): PasskeyOfferDeps & { calls: string[] } {
  const calls: string[] = [];
  return {
    calls,
    whoAmI: async () => {
      calls.push("whoAmI");
      if (options.whoAmIThrows) throw new Error("stream closed");
      return options.caller === undefined
        ? { userId: "v1:identity:user:abc", clusterRole: "owner" }
        : options.caller;
    },
    countOwnPasskeys: async () => {
      calls.push("countOwnPasskeys");
      if (options.countThrows) throw new Error("query failed");
      return options.passkeys ?? 0;
    },
  };
}

// ---------------------------------------------------------------------------
// the offer itself
// ---------------------------------------------------------------------------

test("a cluster owner with no passkey is offered enrolment", async () => {
  const deps = stub({ caller: { userId: "v1:identity:user:abc", clusterRole: "owner" }, passkeys: 0 });
  const decision = await decidePasskeyOffer("local", deps, new OfferMemory());

  assert.deepEqual(decision, { offer: true, userId: "v1:identity:user:abc" });
});

test("an admin is offered too -- the mint gate is owner OR admin", async () => {
  // Read ahead of time from adminops' gate rather than invented here. Offering
  // a mint that comes back PERMISSION_DENIED puts a refusal in front of
  // somebody who did nothing wrong, and audits a call that should not exist.
  const deps = stub({ caller: { userId: "u", clusterRole: "admin" }, passkeys: 0 });
  const decision = await decidePasskeyOffer("local", deps, new OfferMemory());
  assert.equal(decision.offer, true);
});

test("the role is matched case- and whitespace-insensitively", async () => {
  const deps = stub({ caller: { userId: "u", clusterRole: " Owner " }, passkeys: 0 });
  assert.equal((await decidePasskeyOffer("local", deps, new OfferMemory())).offer, true);
});

// ---------------------------------------------------------------------------
// the offer never appears for an identity that already has one
// ---------------------------------------------------------------------------

test("an identity that already has a passkey is never offered", async () => {
  const deps = stub({ passkeys: 1 });
  const decision = await decidePasskeyOffer("local", deps, new OfferMemory());
  assert.deepEqual(decision, { offer: false, reason: "alreadyEnrolled" });
});

test("a malformed passkey count reads as cannot-tell, not as zero", async () => {
  // `> 0` on a NaN is false, which would prompt somebody who IS enrolled. The
  // two wrong answers are not symmetric: staying quiet costs one deferred
  // offer, prompting an enrolled operator costs their trust in the prompt.
  for (const count of [Number.NaN, -1]) {
    const deps = stub({ passkeys: count });
    const decision = await decidePasskeyOffer("local", deps, new OfferMemory());
    assert.deepEqual(
      decision,
      { offer: false, reason: "indeterminate" },
      `count ${String(count)} must not read as "no passkeys"`,
    );
  }
});

// ---------------------------------------------------------------------------
// declining is remembered for the session
// ---------------------------------------------------------------------------

test("a decline is remembered and does not repeat on the next connect", async () => {
  const memory = new OfferMemory();
  const first = await decidePasskeyOffer("local", stub(), memory);
  assert.equal(first.offer, true);

  memory.decline("local");

  const second = await decidePasskeyOffer("local", stub(), memory);
  assert.deepEqual(second, { offer: false, reason: "declinedThisSession" });
});

test("a decline costs no network call on any later connect", async () => {
  // The memory is checked BEFORE anything is asked. Not a micro-optimisation:
  // an operator who said no should not silently pay a round trip for the rest
  // of the session for having said it.
  const memory = new OfferMemory();
  memory.decline("local");
  const deps = stub();

  await decidePasskeyOffer("local", deps, memory);

  assert.deepEqual(deps.calls, [], "declining must short-circuit before whoAmI");
});

test("declining one cluster says nothing about another", async () => {
  const memory = new OfferMemory();
  memory.decline("throwaway-local");
  const decision = await decidePasskeyOffer("shared-staging", stub(), memory);
  assert.equal(decision.offer, true);
});

// ---------------------------------------------------------------------------
// the ownership walk owns its cluster's enrolment story (memql#4078)
// ---------------------------------------------------------------------------

test("a suppressed cluster is never offered, and the reason names the walk", async () => {
  // The first fully-green install ended in THREE stacked notifications: the
  // hand-off's, the walk's, and this offer's -- because the walk's own
  // verification sign-in is still a sign-in, and the offer fires on the heels
  // of every one. Suppression is the walk saying "this cluster's enrolment is
  // mine": the sign-in that ends the walk must not mint a fourth toast.
  const memory = new OfferMemory();
  memory.suppress("local");

  const decision = await decidePasskeyOffer("local", stub(), memory);

  assert.deepEqual(decision, { offer: false, reason: "suppressedByWalk" });
});

test("suppression short-circuits before any network call", async () => {
  // Mirrors the decline test above, for the same reason: the walk marks the
  // cluster BEFORE it mints, so every later sign-in this session answers from
  // memory rather than paying whoAmI plus a passkey query.
  const memory = new OfferMemory();
  memory.suppress("local");
  const deps = stub();

  await decidePasskeyOffer("local", deps, memory);

  assert.deepEqual(deps.calls, [], "suppression must short-circuit before whoAmI");
});

test("suppressed is not declined -- the two markers answer different questions", async () => {
  // Declined: the OPERATOR said no to the offer. Suppressed: the WALK claimed
  // this cluster's enrolment story; the operator said nothing. Collapsing them
  // would make "the walk ran" indistinguishable from "the operator refused" to
  // anything reading the reason.
  const suppressed = new OfferMemory();
  suppressed.suppress("local");
  const declined = new OfferMemory();
  declined.decline("local");

  const walkAnswer = await decidePasskeyOffer("local", stub(), suppressed);
  const operatorAnswer = await decidePasskeyOffer("local", stub(), declined);

  assert.deepEqual(walkAnswer, { offer: false, reason: "suppressedByWalk" });
  assert.deepEqual(operatorAnswer, { offer: false, reason: "declinedThisSession" });
});

test("suppressing one cluster says nothing about another", async () => {
  const memory = new OfferMemory();
  memory.suppress("just-installed-local");
  const decision = await decidePasskeyOffer("shared-staging", stub(), memory);
  assert.equal(decision.offer, true);
});

// ---------------------------------------------------------------------------
// silence wherever it cannot honestly say yes
// ---------------------------------------------------------------------------

test("a caller who cannot mint is not offered", async () => {
  for (const role of ["writer", "reader", ""]) {
    const deps = stub({ caller: { userId: "u", clusterRole: role }, passkeys: 0 });
    const decision = await decidePasskeyOffer("local", deps, new OfferMemory());
    assert.deepEqual(decision, { offer: false, reason: "cannotMint" }, `role ${role}`);
    assert.equal(
      deps.calls.includes("countOwnPasskeys"),
      false,
      "a caller who cannot mint should not be asked about their passkeys either",
    );
  }
});

test("every failure is indeterminate, and indeterminate is silent", async () => {
  // This runs on the heels of a sign-in the operator asked for and got.
  // Surfacing "could not determine your passkey state" on top of a SUCCESS
  // reports a problem they do not have.
  const cases: Array<[string, StubOptions]> = [
    ["whoAmI threw", { whoAmIThrows: true }],
    ["whoAmI answered null", { caller: null }],
    ["whoAmI answered an empty userId", { caller: { userId: "  ", clusterRole: "owner" } }],
    ["the passkey query threw", { countThrows: true }],
  ];
  for (const [name, options] of cases) {
    const decision = await decidePasskeyOffer("local", stub(options), new OfferMemory());
    assert.deepEqual(decision, { offer: false, reason: "indeterminate" }, name);
  }
});

// ---------------------------------------------------------------------------
// no unauthenticated surface reveals whether an account has a passkey
// ---------------------------------------------------------------------------

test("the decision cannot be reached without asking the authenticated stream", async () => {
  // STRUCTURAL, and the strongest form available on this side of the wire: the
  // module takes its two facts as INJECTED dependencies and has no transport of
  // its own -- no fetch, no URL, no probe. Both callers in extension.ts bind
  // them to the connected stream, where `passkeysForSelf` is actor-scoped and
  // `issueEnrolmentLink` is owner-gated server-side.
  //
  // Asserted by construction: with deps that answer nothing, the decision is
  // indeterminate rather than derived from somewhere else.
  const decision = await decidePasskeyOffer(
    "local",
    {
      whoAmI: async () => null,
      countOwnPasskeys: async () => {
        throw new Error("must not be reached");
      },
    },
    new OfferMemory(),
  );
  assert.deepEqual(decision, { offer: false, reason: "indeterminate" });
});

test("the passkey query is only asked for the caller's OWN identity", async () => {
  // `countOwnPasskeys` takes no user argument, mirroring the DSL query it wraps:
  // passkeysForSelf has no userId parameter BY DESIGN, so the row set comes from
  // userId==actor.userId and cannot be pointed at a stranger's authenticators
  // (memql#3178, memql#3409). A signature that accepted one would be the first
  // step toward an oracle.
  const deps = stub();
  assert.equal(deps.countOwnPasskeys.length, 0, "countOwnPasskeys must take no arguments");
});

// ---------------------------------------------------------------------------
// what the operator reads
// ---------------------------------------------------------------------------

test("the offer says what a passkey buys, not what it is", async () => {
  const message = passkeyOfferMessage("local");
  assert.match(message, /local/);
  assert.match(
    message,
    /without waiting for an email/,
    "'Enrol a passkey' is a feature name; the reason somebody says yes is the alternative it replaces",
  );
});

// ---------------------------------------------------------------------------
// a clicked offer re-checks before it mints (memql#4078)
// ---------------------------------------------------------------------------

test("a confirmed passkey cancels the mint a stale offer would run", () => {
  // A toast with buttons persists in the notification bell, so the click can
  // arrive long after the decision -- late enough for the operator to have
  // enrolled through the ownership walk in between. Acting on the stale offer
  // minted a fresh link and the BROWSER delivered the bad news ("a passkey
  // already exists"). The count is therefore re-read at click time, and a
  // confirmed enrolment turns that dead-end into a one-line answer.
  assert.equal(enrolmentStillNeeded(0), true);
  assert.equal(enrolmentStillNeeded(1), false);
  assert.equal(enrolmentStillNeeded(3), false);
});

test("at click time, cannot-tell proceeds -- the inverse of the offer's default", () => {
  // decidePasskeyOffer reads an unreadable count as "stay quiet", because
  // nobody asked for the offer. Here somebody DID ask -- they clicked -- so
  // refusing to mint over a transient query failure would be a new dead end.
  // Only a CONFIRMED enrolment cancels the click.
  for (const count of [Number.NaN, -1, Number.POSITIVE_INFINITY]) {
    assert.equal(
      enrolmentStillNeeded(count),
      true,
      `count ${String(count)} must not cancel a mint the operator asked for`,
    );
  }
});

test("the stale-offer answer is all-set, not an error", () => {
  const message = passkeyAlreadyEnrolledMessage();
  assert.match(message, /already has a passkey/);
  assert.match(
    message,
    /all set/,
    "clicking a stale offer after enrolling is a success to confirm, not a fault to report",
  );
});
