import { useState, type ReactNode } from "react";

import { Button, Rail, type Stop } from "../kit";
import { canConfigure } from "../kit/ReadinessStates";
import { useSetupFacts } from "../apps/setup/context";
import { InferenceStop } from "../apps/setup/InferenceStop";
import { ModuleStop } from "../apps/setup/ModuleStop";
import { PasskeyStop } from "../apps/setup/PasskeyStop";
import { PASSKEY_STOP, drawnState, type SetupStop } from "../apps/setup/stops";
import type { ModuleId } from "../system/modules";
import { useSession } from "./access";
import { useOs } from "./state";
import { Mark } from "./Mark";

// THE CORE GATE (design record 2026-09-07-core-gate-and-honest-install, D1,
// D2 and D6).
//
// ===========================================================================
// AN OS SURFACE, NOT AN ENGINE REFUSAL
// ===========================================================================
// The engine's per-call refusals are already the enforcement. Refusing the
// stream instead would block the very reads the wizard needs, and would take
// from a viewer the feed that tells them to go and ask an owner. The readiness
// rows are readable by every signed-in person and writable only by the engine,
// which is exactly what lets a client-side gate be honest for every role.
//
// ===========================================================================
// IT KEYS ON INFERENCE ALONE (D2)
// ===========================================================================
// Storage is always configured on a local cluster and mail is log-only there
// by design (memql#4477), and neither is needed to use the OS. The other core
// stops render in the same rail, unlit and non-blocking, and stay with the
// setup widget after the gate lifts. The lead sentence says so out loud,
// because a rail of four stops with only one of them holding the door is
// otherwise a screen that looks like it wants all four.
//
// ===========================================================================
// THREE SILENCES, AND ONLY ONE OF THEM HOLDS ANYBODY
// ===========================================================================
//   THE FEED HAS NOT SAID   unloaded, absent, `unreported`, `configured`,
//                           `partial`. The desk OPENS. `unreported` is the
//                           sharp one: a broken cluster is not an unconfigured
//                           one, and a gate that claimed it was would lock
//                           somebody out of the Cluster app they need in order
//                           to go and find out why nothing is reporting.
//   THE LADDER OR THE       nothing at all is drawn. The cluster IS
//   IDENTITY HAS NOT SAID   unconfigured, so the desk must not open -- but
//                           which VARIANT is not decided, and telling an owner
//                           to go and find an owner is worse than a beat of
//                           ground.
//   UNCONFIGURED            the one state that holds.
//
// The PASSKEY read is a fourth wait and it is not one of these: by the time it
// is outstanding the verdict is already in, so the surface draws and only the
// rail waits.
//
// Nothing is dismissed and nothing is remembered in a browser: the verdict IS
// the state, so there is no Skip and nothing to remember. It lifts on a feed
// change with no reload, and a door that is configured but not live -- a
// laptop asleep -- lifts it: configuration opens the OS, presence decides a
// call.
export function CoreGate({ onSignOut, children }: { onSignOut: () => void; children: ReactNode }) {
  const { access, ladderLoaded, readiness } = useSession();
  const { state } = useOs();
  const facts = useSetupFacts();
  const role = access?.role ?? "";
  const [override, setOverride] = useState<string | null>(null);

  // ONLY POSITIVE EVIDENCE HOLDS ANYBODY, and this one line is every silence
  // at once. An unloaded feed answers `null` for every module (`of` reads a
  // map built only when `loaded`), an absent feed answers `null`, an
  // unreported module answers `unreported`, and a configured one answers
  // `configured`. All of them open the desk; only a loaded feed SAYING
  // `unconfigured` does not.
  //
  // FAIL-OPEN ON THE FEED, which costs one flash and buys another. A shell
  // that drew nothing until the feed seeded would put a blank ground in front
  // of every person on every boot of every cluster, to spare the rare
  // unconfigured one a brief desk. The record's own words for this are "the
  // gate draws nothing, the shell opens", and it is the same direction
  // `gateFor` takes for an unknown verdict and `access.tsx` takes for an
  // absent feed: a shell that does not yet know must show the app rather than
  // a screen the person cannot dismiss.
  if (readiness?.of("ai")?.state !== "unconfigured") return <>{children}</>;

  // A HOLD, NOT A PRISON. The gate's own acts OPEN AN APP -- Fleet to pair a
  // machine, Settings to set a provider up -- and a window is drawn by the
  // desk this renders in place of. So while any window is open the desk is
  // drawn and the gate steps aside; close it and the gate returns, unless the
  // door it sent them for is now open.
  //
  // ANY window rather than a named one, because the desk draws all of them and
  // half a desk is not a thing this shell has. That is safe as the whole
  // condition: the stored document never carries windows (state.tsx), so a
  // boot always starts with none, and nothing behind the gate can open one --
  // the only way this is non-empty is an act the person took on this screen.
  if (Object.keys(state.shell.windows).length > 0) return <>{children}</>;

  // FAIL-CLOSED ON THE LADDER AND ON THE IDENTITY, and only here, where it is
  // the whole question. The cluster IS unconfigured -- that much is decided --
  // but WHICH VARIANT to draw is not.
  //
  // `ladderLoaded` and `access` are two INDEPENDENT reads, fired from separate
  // effects in Shell.tsx with no ordering between them, so the ladder landing
  // says nothing about whether the identity has. Reading `access?.role
  // ?? ""` while it is still null hands an OWNER the reader variant -- "an
  // owner or developer has to set up inference", to the owner, with Sign out
  // as the only control. Both reads, or neither variant.
  if (ladderLoaded !== true || access === null) return null;
  // Bound once so `bodyFor` below reads the narrowed value: TypeScript does
  // not carry a narrowing through a closure, and the alternative is a non-null
  // assertion at the one place a wrong answer would be silent. Non-null is
  // safe here and nowhere else: the line above returned unless `of` answered.
  const feed = readiness!;
  const couldNotCheck = feed.of("ai")?.unknown.length ?? 0;

  if (!canConfigure(role)) return <ToldVariant onSignOut={onSignOut} />;

  // THE RAIL WAITS FOR ITS OWN READING, and the chrome above it does not.
  //
  // The stops need this person's passkeys, which is a second read that lands
  // after the feed. The headline and the sentence are already TRUE by the time
  // the `ai` verdict says so, and holding them back would put a blank screen
  // in front of the one person who can act. So the surface draws, and the rail
  // arrives beneath text that does not move.
  const stops = facts !== null && facts.known ? facts.stops : [];
  const openStop = openStopForGate(stops, override);
  const drawn: Stop[] = stops.map((stop) => ({
    id: stop.id,
    name: stop.name,
    state: drawnState(stop, openStop),
    // SAY IT ONCE (interface rule 7). The inference stop's body IS its
    // sentence enumerated -- three named doors -- so printing the sentence
    // above them puts the same answer on the screen twice and pushes the act
    // below the fold. The widget's rail makes the same exception for the same
    // stop.
    sentence: stop.id === "ai" ? undefined : stop.sentence,
    answer: stop.answer,
    body: bodyFor(stop),
    // A SETTLED STOP IS NOT A DISCLOSURE: a chevron promises something behind
    // it, and one that opens an empty body is worse than no chevron.
    openable: stop.state === "waiting",
  }));

  return (
    <div className="os-core-gate" data-os-core-gate="held">
      <div className="os-core-gate-column">
        <span className="os-core-gate-mark" aria-hidden>
          <Mark className="os-ask-mark" />
        </span>
        <h1 className="os-core-gate-head">Set up this cluster</h1>
        <p className="os-core-gate-body">
          MemQL needs a way to reach a model before anyone can use it. The other steps can wait
          &mdash; this screen goes as soon as inference is set up.
        </p>
        {/* THE ONE THING THE VERDICT CANNOT SAY FOR ITSELF (memql#5259). The
            nodes that voted found no door; a node that could not read the
            fleet did not vote, and an owner looking at a held gate deserves to
            know the answer is not every node's. One quiet line, no node ids --
            the Cluster app's Modules detail names them -- and never on a gate
            the voters opened. */}
        {couldNotCheck > 0 ? (
          <p className="os-core-gate-note" data-os-core-gate-unknown>
            {couldNotCheck === 1 ? "One node" : `${couldNotCheck} nodes`} could not read the fleet
            just now, so this may change on its own.
          </p>
        ) : null}
        {drawn.length > 0 ? (
          <Rail
            stops={drawn}
            label="Set up this cluster"
            openStop={openStop}
            onOpenStop={(id) => setOverride(id)}
          />
        ) : null}
        {/* The way out, and it is not one of the acts above. An owner held on
            a surface whose only exit is closing the tab is a small trap; a
            Sign out seated among the stops would read as one of them (rule
            6). So it sits under its own hairline, muted, last. */}
        <div className="os-core-gate-out">
          <Button onClick={onSignOut}>Sign out</Button>
        </div>
      </div>
    </div>
  );

  function bodyFor(stop: SetupStop) {
    // Only an outstanding stop has a body -- the same rule `openable` states,
    // written once as a guard so a stop that stops being openable cannot keep
    // a body nothing can reach.
    if (stop.state !== "waiting") return undefined;
    if (stop.id === PASSKEY_STOP) return <PasskeyStop identityUrl={facts?.identityUrl ?? ""} />;
    const id = stop.id as ModuleId;
    if (id === "ai") return <InferenceStop />;
    return <ModuleStop id={id} verdict={feed.of(id)} />;
  }
}

/**
 * What everybody else gets (D6).
 *
 * THE SAME COLUMN, WITH LESS IN IT. Two shapes for one situation would read as
 * two different kinds of problem, which is the argument `.os-setup` already
 * makes about mirroring `.os-rank-refused`.
 *
 * THE HEAD AND THE SENTENCE DO NOT REPEAT EACH OTHER. The record states the
 * whole thing as one sentence -- "MemQL is not set up yet. An owner or
 * developer has to set up inference before anyone can use it." -- and printing
 * that under a headline saying the first half is rule 7 broken. So the
 * headline IS the first half and the paragraph is the rest; together they are
 * the record's sentence and neither says anything twice.
 *
 * IT DOES NOT NAME THE OWNER. A person's name is not the cluster's to publish,
 * and the sentence is exactly as actionable without one.
 *
 * The third line is the one thing a person on this screen cannot otherwise
 * know: that they do not have to keep reloading. It is true because the
 * readiness feed is live and this component re-renders on it, which is what
 * the "lifts on a feed change" case asserts.
 */
function ToldVariant({ onSignOut }: { onSignOut: () => void }) {
  return (
    <div className="os-core-gate" data-os-core-gate="told">
      <div className="os-core-gate-column">
        <span className="os-core-gate-mark" aria-hidden>
          <Mark className="os-ask-mark" />
        </span>
        <h1 className="os-core-gate-head">MemQL is not set up yet</h1>
        <p className="os-core-gate-body">
          An owner or developer has to set up inference before anyone can use it.
        </p>
        <p className="os-core-gate-note">This page opens by itself once they have.</p>
        <div className="os-core-gate-out">
          <Button onClick={onSignOut}>Sign out</Button>
        </div>
      </div>
    </div>
  );
}

/**
 * Which stop the gate has open.
 *
 * THE INFERENCE STOP OPENS FIRST HERE, and that is the one place the gate
 * departs from the widget's rail. `openStopFor` opens the first OUTSTANDING
 * stop, which on a fresh cluster is the passkey -- right on a desk, wrong on a
 * surface that exists BECAUSE inference is missing and that nothing else will
 * lift.
 *
 * The passkey-first LAW is untouched: the passkey stop is still first in the
 * rail and still carries the sentence that says why. What moves is the
 * disclosure, to the stop this surface is about. A person who opens another
 * stop keeps it open while it is outstanding, exactly as the widget's rail
 * behaves, and closing the open one is honoured.
 */
function openStopForGate(stops: readonly SetupStop[], override: string | null): string {
  if (override !== null) {
    if (override === "") return "";
    const held = stops.find((s) => s.id === override);
    if (held !== undefined && held.state === "waiting") return override;
  }
  const ai = stops.find((s) => s.id === "ai");
  if (ai !== undefined && ai.state === "waiting") return "ai";
  return stops.find((s) => s.state === "waiting")?.id ?? "";
}
