import { useEffect, useState, type ReactNode } from "react";

import { Button, Rail, type Stop } from "../kit";
import { EntryLayout } from "../kit/EntryLayout";
import { EntryPending, useEntryTransition } from "../kit/EntryPending";
import { bootFinished, bootReached } from "../kit/BootCurtain";
import { canConfigure, useAppReach } from "../kit/ReadinessStates";
import { API_KEYS_ADD_PART } from "../apps/settings/apikeys/ApiKeysSection";
import { holds } from "../system/roles";
import { useSetupFacts } from "../apps/setup/context";
import { InferenceStop } from "../apps/setup/InferenceStop";
import { ModuleStop } from "../apps/setup/ModuleStop";
import { PasskeyStop } from "../apps/setup/PasskeyStop";
import { PASSKEY_STOP, drawnState, type SetupStop } from "../apps/setup/stops";
import type { ModuleId } from "../system/modules";
import { useSession } from "./access";
import { AccessHeld } from "./AccessHeld";
import { useKeyLetsIn } from "./keyLetsIn";
import { useOs } from "./state";
import { Mark } from "./Mark";
import { DesktopAccessContext } from "./desktopAccessContext";
import { useDesktopAccess } from "./useDesktopAccess";

// THE CORE GATE (design record 2026-09-07-core-gate-and-honest-install, D1,
// D2 and D6; design record 2026-11-24-desktop-access, sections 2 and 4).
//
// ===========================================================================
// TWO QUESTIONS, EACH FOR DIFFERENT PEOPLE
// ===========================================================================
// THE PERSON'S OWN MODEL ACCESS, for everybody but the cluster's owners and
// developers. Model work runs on a person's own access, in Ask and out of it,
// so somebody with none set up would get a desktop whose features fail one at
// a time. `desktopAccess` answers for the caller (useDesktopAccess); a person
// it holds gets the held page (AccessHeld) in place of everything, whatever
// the cluster's own verdict and whatever windows are open. It admits anybody
// with something set up, asleep or out of credit included, and anybody it
// cannot answer for: a check that fails is no evidence that a person lacks
// access, and the model calls are refused on the server either way. So the
// cluster's verdict holds nobody else, and the page everybody else used to
// get on an unconfigured cluster ("MemQL is not set up yet") is gone.
//
// THE CLUSTER'S READINESS, for owners and developers alone. They are exempt
// from the first question (the engine reads their STORED role) and keep the
// setup screen below.
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
// INITIAL ENTRY WAITS FOR ITS DESTINATION
// ===========================================================================
// Briefly hold the shared entry frame while readiness, access and setup facts
// arrive, and, for anybody not already known to be an owner or developer,
// until the first access check settles. The wait is bounded: three seconds
// from the gate's mount (`waitExpired`). A held person whose first answer
// lands within it sees the page and not the desktop; an answer after it
// replaces the desktop with the page. A check that fails settles too, and lets
// the person in at once. Once entered, refreshes never replace the page with a
// loading frame. A missing feed still opens the desktop after the bound: an
// outage is not evidence that the cluster needs configuration.
//
// Nothing is dismissed and nothing is remembered in a browser: the verdict IS
// the state, so there is no Skip and nothing to remember. It lifts on a feed
// change with no reload, and a door that is configured but not live -- a
// laptop asleep -- lifts it: configuration opens the OS, presence decides a
// call.
//
// ===========================================================================
// AN API KEY THAT CAN PAY LETS ITS HOLDER IN (owner decision 2026-11-23)
// ===========================================================================
// Model access is bring-your-own, and a person's API key is a way to a model
// that the cluster's readiness does not count: the feed is the cluster's, the
// same for everybody, and a key is one person's. So the verdict alone does not
// hold an owner or developer whose own key, or one lent to them, can pay: that
// is asked of `inferenceStatus` for them (useKeyLetsIn) whenever this would
// show. (For everybody else a key is one of the sources the access check
// counts.) The cluster's verdict is unchanged. The screen also offers adding
// one: Settings > API keys, opened straight into its add wizard. Somebody
// withheld from adding keys (the `app:settings/apikeys` part) is offered
// nothing they cannot finish.
export function CoreGate({ onSignOut, children, waitForReadiness = true }: {
  onSignOut: () => void; children: ReactNode; waitForReadiness?: boolean;
}) {
  // The one access read, shared with the desk through a context: Ask words its
  // refusals from the same sources and makes no read of its own.
  const desktopAccess = useDesktopAccess(true);
  return (
    <DesktopAccessContext.Provider value={desktopAccess}>
      <CoreGateBody onSignOut={onSignOut} waitForReadiness={waitForReadiness} desktopAccess={desktopAccess}>{children}</CoreGateBody>
    </DesktopAccessContext.Provider>
  );
}

function CoreGateBody({ onSignOut, children, waitForReadiness, desktopAccess }: {
  onSignOut: () => void; children: ReactNode; waitForReadiness: boolean; desktopAccess: ReturnType<typeof useDesktopAccess>;
}) {
  const { access, ladderLoaded, readiness } = useSession();
  const { state } = useOs();
  const facts = useSetupFacts();
  const role = access?.role ?? "";
  // The cluster's owners and developers: the setup screen is theirs, and the
  // engine's access check exempts them by their stored role.
  const exempt = canConfigure(role);
  const [override, setOverride] = useState<string | null>(null);

  const [entered, setEntered] = useState(false);
  const [waitExpired, setWaitExpired] = useState(false);
  const unconfigured = readiness?.of("ai")?.state === "unconfigured";
  const windowsOpen = Object.keys(state.shell.windows).length > 0;
  // Asked with the verdict while it is not yet known who is looking, as it was
  // before the question became an owner's or developer's alone, so an owner's
  // way in does not wait on the identity read and then on this one. Once the
  // identity names somebody else, it is no longer asked and nothing waits on it.
  const keyAnswer = useKeyLetsIn(unconfigured && (exempt || access === null), windowsOpen);
  const settings = useAppReach("settings");
  // Adding a key is offered only where it can be finished: the part, and a
  // Settings window with its API keys section to open.
  const addKey = holds("execute", API_KEYS_ADD_PART) && settings.canOpenWindows && settings.sections.includes("apikeys")
    ? () => settings.open("apikeys", { addKey: true })
    : undefined;
  // The access check is waited for unless the person is already known to be
  // an owner or developer, whom it does not hold: their entry is what it was.
  const resolving = readiness != null && (!readiness.loaded || (!exempt && !desktopAccess.settled) ||
    (unconfigured && (ladderLoaded !== true || access === null || (exempt && (keyAnswer === "unknown" || facts?.known !== true)))));
  const waiting = waitForReadiness && !entered && !waitExpired && resolving && Object.keys(state.shell.windows).length === 0;
  useEffect(() => {
    if (!waiting) { setEntered(true); return; }
    const timer = setTimeout(() => setWaitExpired(true), 3000);
    return () => clearTimeout(timer);
  }, [waiting]);
  const entryTransition = useEntryTransition(waiting || (unconfigured && Object.keys(state.shell.windows).length === 0 && (ladderLoaded !== true || access === null)));
  // The first load's curtain: the cluster's first facts are in, then the destination is drawn.
  useEffect(() => { if (!waiting) bootReached(2); }, [waiting]);
  useEffect(() => { if (!entryTransition.show) bootFinished(); }, [entryTransition.show]);
  if (entryTransition.show) return <EntryPending leaving={entryTransition.leaving} />;

  // HELD: the page replaces everything, the open-window escape below included.
  // It opens no window (a key is added on the page itself), and a window left
  // open from before access was taken away does not keep the desk.
  if (desktopAccess.state === "held") {
    return (
      <AccessHeld
        reference={desktopAccess.reference}
        canAddKey={holds("execute", API_KEYS_ADD_PART)}
        onSignOut={onSignOut}
        onAdded={desktopAccess.recheck}
      />
    );
  }

  // Only a reported unconfigured verdict holds the desktop after entry.
  if (readiness?.of("ai")?.state !== "unconfigured") return <>{children}</>;

  // FAIL-CLOSED ON THE LADDER AND ON THE IDENTITY, and only here, where it is
  // the whole question. The cluster IS unconfigured -- that much is decided --
  // but WHO is looking is not, and it decides between the setup screen and the
  // desk: the verdict holds owners and developers only.
  //
  // `ladderLoaded` and `access` are two INDEPENDENT reads, fired from separate
  // effects in Shell.tsx with no ordering between them, so the ladder landing
  // says nothing about whether the identity has. Reading `access?.role ?? ""`
  // while it is still null would read an OWNER as somebody else and open the
  // desk on a cluster they have to set up. The shared entry transition above
  // holds the frame until both have landed (or a window is open, which the
  // desk draws either way), so here the role is the person's.
  if (!exempt) return <>{children}</>;
  // An owner or developer whose own API key can pay.
  if (keyAnswer === "yes") return <>{children}</>;

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
  if (windowsOpen) return <>{children}</>;

  // Bound once so `bodyFor` below reads the narrowed value: TypeScript does
  // not carry a narrowing through a closure, and the alternative is a non-null
  // assertion at the one place a wrong answer would be silent. Non-null is
  // safe here and nowhere else: the verdict check above returned unless `of`
  // answered.
  const feed = readiness!;
  const couldNotCheck = feed.of("ai")?.unknown.length ?? 0;

  // If the initial wait expires before the passkey read completes, the known
  // setup heading can still appear. The rail needs its own facts and must not
  // present an unknown passkey as a missing one.
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
    <EntryLayout><main className="os-core-gate" data-os-core-gate="setup">
      <div className="os-core-gate-column">
        <span className="os-core-gate-mark" aria-hidden>
          <Mark className="os-ask-mark" />
        </span>
        <h1 className="os-core-gate-head">Set up this cluster</h1>
        <p className="os-core-gate-body">
          Choose how MemQL reaches a model. The rest can wait; this screen goes once one is set up.
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
        {/* A PERSON'S OWN WAY IN, under the cluster's doors and not among
            them: a key sets nothing up for anybody else, which the line says,
            so it is not one of the stop's doors and the button is not its
            primary act. */}
        {addKey ? (
          <div className="os-core-gate-key">
            <p className="os-core-gate-note">An API key of your own lets you in, and nobody else.</p>
            <Button onClick={addKey}>Add an API key</Button>
          </div>
        ) : null}
        {/* The way out, and it is not one of the acts above. An owner held on
            a surface whose only exit is closing the tab is a small trap; a
            Sign out seated among the stops would read as one of them (rule
            6). So it sits under its own hairline, muted, last. */}
        <div className="os-core-gate-out">
          <Button onClick={onSignOut}>Sign out</Button>
        </div>
      </div>
    </main></EntryLayout>
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
