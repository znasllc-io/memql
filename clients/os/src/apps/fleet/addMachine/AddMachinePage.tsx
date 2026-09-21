import { localCockpitInstall } from "./localInstall";
import { useEffect, useState } from "react";

import { useSession } from "../../../chrome/access";
import { Monitor } from "lucide-react";

import { Caption, CopyField, stopIsReachable, type Stop } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import { useNow } from "../../../kit/useNow";
import { Wizard } from "../../../kit/Wizard";
import { openStopFor, waitedLong, type ActId, type StopId } from "./flow";
import { uninstallCommand, workerClusterUrl } from "./install";
import { ChecksStop } from "./stops/Checks";
import { ConnectStop } from "./stops/Connect";
import { InstallStop } from "./stops/Install";
import { MachineStop } from "./stops/Machine";
import type { AddMachineFlow } from "./useAddMachineFlow";

// THE GUIDED INSTALL (design record 2026-09-08-cockpit-install-wizard).
//
// ===========================================================================
// THE RAIL IS THE FORM, AND THE ORDER IS A LAW
// ===========================================================================
// This is the compose reading (Deployables, epic memql#4885) on the Fleet:
// the page replaces the list (interface rule 11), the stops carry the form
// and then the answers, the bar on the window's bottom edge carries the state
// in words and the acts legal from it (rule 12). What is different from the
// first-run rail is that the order here is a law -- no command without a
// token, no connection without the command having run, no check without a
// registration -- so unreached stops draw `ahead`, and the rail moves on
// what HAPPENS: a mint, a registration, a heartbeat. Never on a click.
//
// ===========================================================================
// TWO LIT MARKS AT ONCE, ONCE
// ===========================================================================
// While the person is in a terminal, Install is `open` (the held ring:
// waiting on you) and Connect is `current` (the pulse: the cluster is
// listening). It is the one moment the rail draws two lit marks, and it is
// the honest picture: you do this; we are listening for that.
//
// Everything this page knows comes from the flow held by FleetApp (D7), so
// the page can be unmounted by a section change and remounted with the
// token still here and the cluster still listening.

export function AddMachinePage({
  flow,
  onLeave,
}: {
  flow: AddMachineFlow;
  /** Leave the page. `openMachineId` names the registration to open in the
   *  list, or "" for none. */
  onLeave: (openMachineId: string) => void;
}) {
  const { config } = useSession();
  const { facts, stops, checks, bar, phase } = flow;

  // THE RAIL OPENS THE STOP THE FLOW IS AT, and a person may open any other
  // reachable stop to re-read it. Their choice is forgotten the moment the
  // flow moves on -- a rail that stayed on "This machine" while the machine
  // connected would hide the thing that just happened.
  const [override, setOverride] = useState<string | null>(null);
  useEffect(() => {
    setOverride(null);
  }, [phase]);
  const computed = openStopFor(stops);
  const chosen = override !== null && stops.some((s) => s.id === override && stopIsReachable(s.state)) ? override : null;
  const openStop = chosen ?? computed;

  const drawn: Stop[] = stops.map((stop) => ({
    id: stop.id,
    name: stop.name,
    state: stop.state,
    sentence: stop.sentence,
    answer: stop.answer,
    body: bodyFor(stop.id),
    // A SETTLED STOP WITH NOTHING BEHIND IT IS NOT A DISCLOSURE. Once the
    // machine has connected, the token is spent and the form is answered on
    // its own line; a chevron there would open an empty body. The Checks
    // stop keeps opening for as long as it is reached, because the repairs
    // live behind it.
    openable: stop.id === "checks" ? stopIsReachable(stop.state) : stop.state === "open" || stop.state === "current",
  }));

  const acts: Act[] = bar.acts.map((act) => ({
    label: act.label,
    tone: act.tone,
    busy: act.busy,
    text: act.text,
    onAct: () => run(act.id),
  }));

  // HOW LONG THE CLUSTER HAS BEEN LISTENING, beside the words that say it is.
  // "Waiting" with no figure cannot tell a moment from ten minutes, and ten
  // minutes is when the Connect step starts naming what usually went wrong.
  // Its own clock, by the second: the flow's `now` ticks every fifteen, and a
  // counter that jumps 0:15 at a time reads as one that has stalled.
  const tick = useNow(phase === "waiting" ? 1000 : 60_000);
  const waited = phase === "waiting" && facts.mintedAt !== null ? elapsed(tick.getTime() - facts.mintedAt.getTime()) : undefined;

  return (
    <Wizard
      className="os-fleet-addpage"
      icon={<Monitor aria-hidden />}
      title="Add a machine"
      lead="Connect a computer to this cluster so it can run work for it."
      /* GOING BACK IS LEAVING, NOT CANCELLING. With a token waiting to be used
         it asks the Leave question -- the token is shown only here -- and
         never revokes anything; before the mint there is nothing to keep, so
         it simply goes. */
      back={{ label: "Machines", onSelect: () => (phase === "connected" ? run("done") : phase === "waiting" ? flow.askLeave() : flow.cancel()) }}
      label="Adding a machine"
      steps={drawn}
      open={openStop}
      onOpen={setOverride}
      status={{ word: bar.state, detail: bar.detail, tone: bar.tone, meta: waited }}
      acts={acts}
      context={{ page: "Add a machine", phase, step: openStop }}
      confirm={bar.question === "" ? null : (
        <div className="os-actbar-confirm os-fleet-leave">
          <p className="os-fleet-leave-question">{bar.question}</p>
          {/* THE UNINSTALL LINE, WITH THE QUESTION THAT REVOKES (D12). A person
              who already ran the install has a worker on that machine retrying
              with a token about to be revoked; a registration that never
              happens has no machine page to get the line from. Leaving keeps
              the token, so that question has nothing to uninstall. */}
          {facts.cancelAsked ? <>
            <Caption>If you already ran the install on the machine, this removes it again:</Caption>
            <CopyField value={uninstallCommand(flow.draft.platform, { userLocal: flow.draft.userLocal, localTest: localCockpitInstall(config.domain), clusterUrl: workerClusterUrl(config.domain) })} label="the uninstall command" />
          </> : null}
        </div>
      )}
    />
  );

  function run(id: ActId): void {
    switch (id) {
      case "cancel":
        flow.cancel();
        return;
      case "mint":
        void flow.mint();
        return;
      case "leave":
        flow.askLeave();
        return;
      case "keepWaiting":
        flow.keepWaiting();
        return;
      case "revokeAndLeave":
        void flow.revokeAndLeave().then(() => {
          // The hook resets itself on success; a refusal keeps the question
          // open with the reason. Either way the page follows the flow.
        });
        return;
      case "leaveKeepToken":
        flow.leaveKeepToken();
        onLeave("");
        return;
      case "open":
        onLeave(flow.finish());
        return;
      case "done":
        flow.finish();
        onLeave("");
        return;
    }
  }

  function bodyFor(id: StopId) {
    switch (id) {
      case "machine":
        // The form is the body only while it is being answered. After the
        // mint the summary is on the line and the stop does not open.
        if (phase !== "describe") return undefined;
        return (
          <MachineStop
            draft={flow.draft}
          localTest={localCockpitInstall(config.domain)}
            onDraft={flow.setDraft}
            connected={facts.connected}
            mintError={facts.mintError}
            onMint={() => {
              if (bar.acts.some((a) => a.id === "mint")) void flow.mint();
            }}
          />
        );
      case "install":
        if (facts.mint === null || phase !== "waiting") return undefined;
        return <InstallStop draft={flow.draft} token={facts.mint.token} domain={config.domain} />;
      case "connect":
        if (phase === "describe" || phase === "minting") return undefined;
        return (
          <ConnectStop
            name={flow.draft.name}
            machine={facts.machine}
            waitedLong={waitedLong(facts)}
            domain={config.domain}
            renameError={flow.renameError}
            now={facts.now}
          />
        );
      case "checks":
        if (phase !== "connected" || facts.machine === null) return undefined;
        return (
          <ChecksStop
            checks={checks}
            machine={facts.machine}
            pulling={flow.pulling}
            pullError={flow.pullError}
            onPullRecommended={() => void flow.pullRecommended()}
            onRetryResponse={flow.retryResponse}
          />
        );
    }
  }
}


/** m:ss, for a wait measured in minutes. */
function elapsed(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}
