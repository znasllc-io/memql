// Whether the cluster panel's deploy actions can be pressed right now.
//
// The bug this fixes: actionsHtml() asked ONE question -- what is the caller's
// role -- so a cluster that was unreachable, or whose credential had expired,
// rendered a full set of live-looking deploy buttons directly beneath a warning
// saying it was not connected. Every one of them was certain to fail.
//
// Two questions, asked in order. Role decides what EXISTS (a control you may
// never use should not be drawn at all); connection state decides what is
// PRESSABLE (a control that is momentarily unusable greys out and names the one
// thing to click). Merging them would lose that distinction, which is the
// difference between "you cannot do this" and "you cannot do this yet".
//
// The verdict comes from src/clusters/status.ts rather than being recomputed
// here, so the row icon and the panel can never disagree about the same cluster.

import test from "node:test";
import assert from "node:assert/strict";

import { actionEnablement } from "../src/deploy/enablement.js";

const REMOTE = { name: "staging" };
const LOCAL = { name: "local", local: true };

test("a connected cluster enables the actions", () => {
  const e = actionEnablement("connected", REMOTE);
  assert.equal(e.enabled, true);
  assert.equal(e.disabledReason, "");
  assert.equal(e.primaryControl, undefined);
});

test("an idle cluster disables them and offers Connect", () => {
  const e = actionEnablement("idle", REMOTE);
  assert.equal(e.enabled, false);
  assert.equal(e.primaryControl, "connect");
  assert.match(e.disabledReason, /staging/, "the reason must name the cluster");
});

test("an unreachable remote cluster offers Connect", () => {
  const e = actionEnablement("failed", REMOTE);
  assert.equal(e.enabled, false);
  assert.equal(e.primaryControl, "connect");
});

test("an unreachable LOCAL cluster offers Repair instead", () => {
  // The distinction is the whole point of carrying `local` in here. A remote
  // cluster that does not answer is somebody else's outage and the only thing
  // this editor can do is retry. A local one is on this machine, installed by
  // this extension, and re-running the install graph over it IS the repair.
  const e = actionEnablement("failed", LOCAL);
  assert.equal(e.enabled, false);
  assert.equal(e.primaryControl, "repair");
});

test("an expired credential offers Sign in, not Connect", () => {
  // memql#3385 drew this distinction for the row icon precisely because the
  // two have completely different next actions; the panel must not collapse it
  // back into "try connecting again", which would fail identically.
  const e = actionEnablement("credential", REMOTE);
  assert.equal(e.enabled, false);
  assert.equal(e.primaryControl, "signIn");
});

test("a credential problem on a local cluster is still a credential problem", () => {
  // `local` only redirects the UNREACHABLE case. A local cluster that is up but
  // whose token expired needs a sign-in, and offering to repair it would rebuild
  // a working cluster to fix a token.
  const e = actionEnablement("credential", LOCAL);
  assert.equal(e.primaryControl, "signIn");
});

test("an unconfigured cluster offers Edit -- there is nothing to connect to", () => {
  // `unconfigured` means an empty endpoint. Connect cannot succeed and Sign in
  // has nowhere to go; the only thing that fixes it is supplying an address.
  const e = actionEnablement("unconfigured", REMOTE);
  assert.equal(e.enabled, false);
  assert.equal(e.primaryControl, "edit");
});

test("a handshake in flight offers nothing to click", () => {
  // Offering Connect during a connect is an invitation to make it worse. The
  // state resolves on its own, so the panel says so and waits.
  const e = actionEnablement("connecting", REMOTE);
  assert.equal(e.enabled, false);
  assert.equal(e.primaryControl, undefined);
});

test("every reason is a whole sentence a tooltip can show verbatim", () => {
  const icons = ["idle", "failed", "credential", "unconfigured", "connecting"] as const;
  for (const icon of icons) {
    const { disabledReason } = actionEnablement(icon, REMOTE);
    assert.notEqual(disabledReason, "", `${icon} must explain itself`);
    assert.match(
      disabledReason,
      /\.$/,
      `${icon}: the reason is rendered as a tooltip with no wrapper, so it ends as a sentence`,
    );
  }
});
