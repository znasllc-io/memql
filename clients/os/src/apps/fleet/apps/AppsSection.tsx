import { useEffect, useState } from "react";

import { Button, Check, Chip, Field, Head, Input, Notice, Panel, Row, Select, Subhead } from "../../../kit";
import { formatFreshness, formatMoment } from "../../../kit/format";
import { useNow } from "../../../kit/useNow";
import { FigureValue } from "../../../cluster/FigureValue";
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

export function AppsSection() {
  const policy = useDelegationPolicy();
  const sessions = useAppSessions();
  const [openSessionId, setOpenSessionId] = useState("");
  const now = useNow(30_000);

  if (openSessionId !== "") {
    return (
      <SessionPage
        sessionId={openSessionId}
        onBack={() => setOpenSessionId("")}
      />
    );
  }

  return (
    <div className="os-fleet">
      <Head title="Apps" meta={sessions.sessions.length}>
        {/* NOT A STANDING REFRESH. Neither read on this screen is live --
            v1:worker:delegationPolicy and v1:worker:appSession carry no
            broadcast rule -- so unlike the Routing and Workbenches sections,
            where the control appears only when the FEED is behind, here it is
            the honest permanent affordance: this surface says when it looked
            and offers to look again. */}
        <Button
          onClick={() => {
            policy.reread();
            sessions.reread();
          }}
        >
          Re-read
        </Button>
      </Head>

      <DelegationPanel state={policy} />

      <Subhead>Delegated runs</Subhead>
      <p className="os-caption">
        {sessions.readAt === null
          ? "Not read yet."
          : `Read ${formatFreshness(sessions.readAt.toISOString(), now)}. These rows are not a live feed -- re-read to see newer runs.`}
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
        <p className="os-caption">
          No app session has run yet. One appears here the first time a task is handed to a local
          app on one of your machines.
        </p>
      ) : null}

      <ul className="os-fleet-sessions" aria-label="Delegated runs">
        {sessions.sessions.map((session) => (
          <li key={session.id}>
            <SessionLine session={session} now={now} onOpen={() => setOpenSessionId(session.id)} />
          </li>
        ))}
      </ul>
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
    <Row
      name={appLabel(session.app)}
      current={session.status === "running" || session.status === "starting"}
      dim={session.status === "cancelled"}
      onOpen={onOpen}
      state={
        <>
          {/* The word, in its own tone -- not a colour alone. "The red one"
              is not something a person can say to a colleague on a call, and
              a screen reader has no colour at all. */}
          <span className="os-fleet-session-status" data-tone={statusTone(session.status)}>
            {session.status}
          </span>
          {/* Subscription spend is the fact this row exists to make visible:
              it does not burn the plan's dollar ceiling. `unknown` is a real
              answer (the app reported nothing) and never folded into either
              side. */}
          <Chip tone={session.billing === "subscription" ? "accent" : "muted"}>
            {session.billing}
          </Chip>
          {/* TOKENS THAT WERE NOT REPORTED ARE ABSENT, NOT ZERO. An app that
              said nothing did not say zero, and a 0 beside "tokens" is a
              measurement -- it reads as a run that spent nothing. */}
          <span className="os-fleet-session-tokens">
            <FigureValue figure={totalTokens(session)} suffix=" tokens" />
          </span>
          <span className="os-caption">{formatFreshness(session.startedAt, now)}</span>
        </>
      }
    />
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
            <strong>Delegation is off.</strong> You have not set a policy, so nothing is handed to
            a local app -- every task runs in the cluster, as it always has. That is the default
            on purpose: an agent running on your own computer should be something you turned on.
            Nothing is written until you save.
          </p>
        </Notice>
      ) : null}

      <fieldset className="os-field-group">
        <legend>The master choice</legend>
        <Check
          checked={draft.preferSubscriptionApps}
          disabled={state.saving}
          onChange={(preferSubscriptionApps) => edit({ preferSubscriptionApps })}
        >
          Delegate eligible tasks to my local apps
        </Check>
        {/* A PREFERENCE WITH A FALLBACK, said where the switch is. Somebody
            reading this must not believe they are switching work OFF: with no
            allowed, signed-in, online machine the task runs in-process, and a
            plan never waits for a laptop to wake up. */}
        <p className="os-caption">
          With this off, nothing below applies. With it on, a task is delegated only when a
          machine with an allowed, signed-in app is online right now -- otherwise it runs in the
          cluster exactly as before. A plan never waits for a laptop to wake up.
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
        <p className="os-caption">
          Order is priority: the FIRST app on this list that a machine actually has wins. An app
          you do not list is never selected, even on a machine that has it -- the list is how you
          say which of your subscriptions to spend.
        </p>
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
              {kind}
            </Check>
          ))}
        </div>
        <p className="os-caption">
          A kind absent from this list runs in the cluster whatever else is true. Turning
          delegation on does not silently opt every kind of work in with it.
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
        Each run gets its own directory under this root. The cockpit refuses a workspace outside
        its own policy roots, so this is a preference the machine still gets to veto.
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
          {state.found ? "Save delegation policy" : "Turn delegation on"}
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

      <p className="os-caption">
        Saved as one write: the whole form goes together, so delegation is never switched on with
        an empty app list -- a policy that routes nothing and says nothing.
      </p>
    </Panel>
  );
}
