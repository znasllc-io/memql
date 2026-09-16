import { useEffect, useState } from "react";
import { FleetTabs, RefreshButton, useFleetScroll } from "../FleetControls";
import { InfoDetail } from "../../../kit/InfoDetail";

import { Button, Check, Switch, EmptyState, Chip, Field, Head, Input, Notice, Panel, Select, Subhead } from "../../../kit";
import { formatFreshness, formatMoment } from "../../../kit/format";
import { useNow } from "../../../kit/useNow";
import { Measure } from "../../../kit/MeasureView";
import {
  appLabel,
  DELEGATABLE_KINDS,
  MAX_CONCURRENT_SESSION_CHOICES,
  RUNNABLE_APPS,
  statusTone,
  totalTokens,
  type AppSessionRow,
} from "../rows";
import { SessionPage } from "./SessionPage";
import { useAppSessions } from "./useAppSessions";
import { useDelegationPolicy, type DelegationPolicyDraft } from "./useDelegationPolicy";

// Apps: when work is handed to a local app on one of your own machines, and
// what happened when it was (epic memql#5009).
//
// Fleet -> Machines already lists each machine's apps and their runnable and
// subscription state. This section is the other two thirds of the story, in
// the order a person asks it: WHEN should we delegate, and WHAT happened.
// They sit in one section rather than two because the policy is what connects
// the machines to the runs -- split apart, the middle one reads as a setting
// rather than as the thing that decided.
//
// RULE 11: THE LIST AND ONE RUN'S TRANSCRIPT NEVER SHARE A SCROLL COLUMN. A
// transcript is tall by nature, so the detail REPLACES this view and carries
// a quiet back-Head, exactly as DeployablePage does. One Head per view; two
// Heads in one scroller is the tell that neither happened.

export function AppsSection({ sessionTarget, navigation }: { sessionTarget?: { id: string; revision: number }; navigation?: { origin: "peer" | "content" | "back"; revision: number } } = {}) {
  const policy = useDelegationPolicy();
  const sessions = useAppSessions();
  const [openSessionId, setOpenSessionId] = useState("");
  useEffect(() => { if (sessionTarget) setOpenSessionId(sessionTarget.id); }, [sessionTarget?.revision]);
  useEffect(() => { if (navigation?.origin === "peer") { setOpenSessionId(""); setView("sessions"); } }, [navigation?.revision]);
  const now = useNow(30_000);

  const [view, setView] = useState<"sessions" | "delegation">("sessions");
  const root = useFleetScroll(openSessionId || view);

  return (
    <div ref={root} className="os-fleet">
    {openSessionId ? <SessionPage sessionId={openSessionId} onBack={() => setOpenSessionId("")} /> : null}
    <div className="fleet-pane-overview" hidden={openSessionId !== ""}>
      <div className="fleet-section-header">
      <Head title="Activity" meta={sessions.sessions.length}>
        {/* NOT A STANDING REFRESH. Neither read on this screen is live --
            v1:worker:delegationPolicy and v1:worker:appSession carry no
            broadcast rule -- so unlike the Routing and Workbenches sections,
            where the control appears only when the FEED is behind, here it is
            the honest permanent affordance: this surface says when it looked
            and offers to look again. */}
        <RefreshButton label="Refresh app activity" busy={sessions.loading || policy.loading} onClick={() => { policy.reread(); sessions.reread(); }} />
      </Head>

      <FleetTabs label="Activity views" value={view} onChange={setView} options={[["sessions", "App sessions"], ["delegation", "Delegation"]]} />
      </div>
      <div hidden={view !== "delegation"}><DelegationPanel state={policy} /></div>
      <div hidden={view !== "sessions"}>

      <Subhead>Delegated runs</Subhead>
      <p className="os-caption">
        {sessions.readAt === null
          ? "Not read yet."
          : `Read ${formatFreshness(sessions.readAt.toISOString(), now)}. Refresh for newer sessions.`}
      </p>

      {sessions.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The delegated runs could not be read."
          next={
            sessions.sessions.length > 0
              ? "The runs below are the last ones that were read."
              : "Nothing was loaded."
          }
          detail={sessions.error}
        />
      )}

      {sessions.loading && sessions.sessions.length === 0 ? (
        <p className="os-caption">Reading your delegated runs.</p>
      ) : null}

      {!sessions.loading && sessions.sessions.length === 0 && sessions.error === "" ? (
        <EmptyState title="No app sessions yet" action={<Button onClick={() => setView("delegation")}>Review delegation</Button>}>Sessions appear when MemQL hands a task to an allowed app on one of your machines.</EmptyState>
      ) : null}

      <ul className="os-fleet-sessions" aria-label="Delegated runs">
        {sessions.sessions.map((session) => (
          <li key={session.id}>
            <SessionLine session={session} now={now} onOpen={() => setOpenSessionId(session.id)} />
          </li>
        ))}
      </ul>
      </div></div>
    </div>
  );
}

function SessionLine({
  session,
  now,
  onOpen,
}: {
  session: AppSessionRow;
  now: Date;
  onOpen: () => void;
}) {
  return (
    <button type="button" className="fleet-activity-record" onClick={onOpen}>
      <span className="fleet-record-identity"><strong>{appLabel(session.app)}</strong><small>{session.runId || session.kind || "App session"}</small></span>
      <span className="os-fleet-session-status" data-tone={statusTone(session.status)}>{session.status}</span>
      <Chip tone={session.billing === "subscription" ? "accent" : "muted"}>{session.billing}</Chip>
      {totalTokens(session).kind === "measured" ? <span className="os-fleet-session-tokens fleet-record-meta"><Measure figure={totalTokens(session)} suffix=" tokens" /></span> : null}
      <span className="fleet-record-meta">{formatFreshness(session.startedAt, now)}</span>
      <span aria-hidden>›</span>
    </button>
  );
}

// ---------------------------------------------------------------------------
// The delegation policy editor
// ---------------------------------------------------------------------------

function DelegationPanel({ state }: { state: ReturnType<typeof useDelegationPolicy> }) {
  const [draft, setDraft] = useState<DelegationPolicyDraft>(state.initial);
  const [touched, setTouched] = useState(false);

  // STALENESS RESOLVES TOWARD THE ROW, but only into an UNTOUCHED draft --
  // the routing editor's rule, and for its reason: a policy saved elsewhere
  // must reach this editor or somebody saves a set assembled from a state the
  // cluster never had, while discarding typing in progress would be worse
  // than either.
  const initialKey = JSON.stringify(state.initial);
  useEffect(() => {
    if (touched) return;
    setDraft(JSON.parse(initialKey) as DelegationPolicyDraft);
    // initialKey IS the value-identity of the initial draft; depending on the
    // object would reset the draft on every render, since the hook memoises a
    // fresh one per policy fold.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialKey, touched]);

  function edit(patch: Partial<DelegationPolicyDraft>) {
    setTouched(true);
    setDraft((held) => ({ ...held, ...patch }));
  }

  const toggleIn = (list: readonly string[], value: string): string[] =>
    list.includes(value) ? list.filter((one) => one !== value) : [...list, value];

  return (
    <Panel label="Delegation">
      <div className="os-head">
        <Subhead>Delegation</Subhead>
        <span className="os-head-meta">
          {state.readAt === null ? "not read yet" : `read ${formatMoment(state.readAt.toISOString())}`}
        </span>
      </div>

      {state.loading ? <p className="os-caption">Reading your delegation policy.</p> : null}

      {state.error === "" ? null : (
        <Notice
          tone="error"
          sentence="Your delegation policy could not be read."
          next="The form below shows the defaults until it loads. Nothing is written until you save."
          detail={state.error}
        />
      )}

      {/* THE ABSENT ROW IS A STATEMENT, NEVER A BLANK FORM. Most people have
          no policy row, and the planner reads that as "never delegate". An
          empty form invites "not configured yet, so some default applies" --
          and here the default IS off, which is the thing worth being
          unambiguous about before an agent runs on somebody's laptop. */}
      {!state.found && !state.loading ? (
        <Notice tone="info">
          <p className="os-notice-line">
            <strong>Delegation is off.</strong> Tasks stay in the cluster until you save a policy.
          </p>
        </Notice>
      ) : null}

      <fieldset className="os-field-group">
        <legend className="os-sr-only">Delegation preference</legend>
        <Switch
          checked={draft.preferSubscriptionApps}
          disabled={state.saving}
          onChange={(preferSubscriptionApps) => edit({ preferSubscriptionApps })}
        >
          Delegate eligible tasks to my local apps
        </Switch>
        {/* A PREFERENCE WITH A FALLBACK, said where the switch is. Somebody
            reading this must not believe they are switching work OFF: with no
            allowed, signed-in, online machine the task runs in-process, and a
            plan never waits for a laptop to wake up. */}
        <p className="os-caption">
          Use your signed-in apps when an allowed machine is online. Otherwise, tasks continue in the cluster. Changes take effect when you save.
        </p>
      </fieldset>

      <fieldset className="os-field-group">
        <legend>Apps, in the order to try them</legend>
        <div className="os-fleet-apps-choices">
          {RUNNABLE_APPS.map((appId) => (
            <Check
              key={appId}
              checked={draft.appOrder.includes(appId)}
              disabled={state.saving}
              onChange={() => edit({ appOrder: toggleIn(draft.appOrder, appId) })}
            >
              {appLabel(appId)}
            </Check>
          ))}
        </div>
        {/* ORDER IS PRIORITY, and it is stated because nothing about a pair of
            checkboxes says so. The chosen order is shown back, so the list is
            not something to infer from click history. */}
        <InfoDetail title="App ordering"><p className="os-caption">
          MemQL tries selected apps in the order shown below. Only these apps may use your subscriptions. Remove and reselect an app to move it to the end.
        </p></InfoDetail>
        <p className="os-caption">
          {draft.appOrder.length === 0
            ? "No app listed, so nothing can be selected even with delegation on."
            : `Tried in this order: ${draft.appOrder.map(appLabel).join(", then ")}.`}
        </p>
      </fieldset>

      <fieldset className="os-field-group">
        <legend>Task kinds that may be delegated</legend>
        <div className="os-fleet-apps-choices">
          {DELEGATABLE_KINDS.map((kind) => (
            <Check
              key={kind}
              checked={draft.eligibleKinds.includes(kind)}
              disabled={state.saving}
              onChange={() => edit({ eligibleKinds: toggleIn(draft.eligibleKinds, kind) })}
            >
              {({ runCommand: "Run commands", fileProcessor: "Process files", callTool: "Use tools", persistResult: "Save results" } as Record<string, string>)[kind] ?? kind}
            </Check>
          ))}
        </div>
        <p className="os-caption">
          Only selected task kinds may use local apps. Other work stays in the cluster.
        </p>
      </fieldset>

      <Field label="Most sessions at once">
        <Select
          id="fleet-apps-concurrency"
          label="Most sessions at once"
          value={String(draft.maxConcurrentSessions)}
          onChange={(next) => edit({ maxConcurrentSessions: Number(next) || 1 })}
        >
          {MAX_CONCURRENT_SESSION_CHOICES.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </Select>
      </Field>
      <p className="os-caption">Across every machine, not per machine.</p>

      <Field label="Workspace root on the machine">
        {/* The kit's Input, not a bare one: `Field` supplies the visible name
            and the control keeps its own hidden label, which is the shell's
            one field size (DESIGN.md rule 5). */}
        <Input
          id="fleet-apps-workspace-root"
          label="Workspace root on the machine"
          value={draft.workspaceRoot}
          disabled={state.saving}
          placeholder="/Users/you/memql-workspaces"
          onChange={(workspaceRoot) => edit({ workspaceRoot })}
        />
      </Field>
      {/* THE MACHINE STILL GETS TO REFUSE IT. This is a preference, and the
          cockpit's own policy.yaml roots are the authority -- saying so here
          is what stops "I set the root and it did not use it" reading as a
          bug in this form. */}
      <p className="os-caption">
        Each session gets its own directory here. The location must also be allowed by Cockpit on that machine.
      </p>

      {state.saveError === "" ? null : (
        <Notice
          tone="error"
          sentence="The delegation policy was not saved."
          next="Nothing was written; what is on screen is your edit, not what the cluster holds."
          detail={state.saveError}
        />
      )}

      <div className="os-head-actions">
        <Button
          tone="primary"
          busy={state.saving}
          busyLabel="Saving..."
          onClick={() => {
            void state.save(draft).then((ok) => {
              // Released ONLY on success, so the row becomes authoritative
              // again. Releasing after a refusal would discard the edits in
              // the same beat as an error saying they were kept.
              if (ok) setTouched(false);
            });
          }}
        >
          {state.found || !draft.preferSubscriptionApps ? "Save delegation policy" : "Turn delegation on"}
        </Button>
        <Button
          disabled={!touched || state.saving}
          onClick={() => {
            setTouched(false);
            setDraft(state.initial);
          }}
        >
          Discard changes
        </Button>
      </div>

      <p role="status" className="os-status-line">
        {state.announcement}
      </p>


    </Panel>
  );
}
