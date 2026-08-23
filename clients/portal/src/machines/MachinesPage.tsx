import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { ErrorMessage } from "../components/StatusMessage";
import {
  Badge,
  Band,
  Button,
  Callout,
  Container,
  EmptyState,
  Field,
  PageHeader,
  Select,
  Skeleton,
  TextInput,
} from "../ui";
import { sessionPath } from "./urls";
import { appLabel, useMachines, type MachineApp } from "./useMachines";
import { useAppSessions, type AppSessionCard } from "./useAppSessions";
import {
  useDelegationPolicy,
  type DelegationPolicy,
} from "./useDelegationPolicy";

// The machines surface (memql#4363): the caller's cockpit machines and the
// local apps each one reports, the delegation policy that decides when a task
// is handed to one, and the delegated runs that resulted.
//
// THE THREE BANDS ARE ONE STORY, in the order a person asks it: what can I
// delegate TO, when should we delegate, and what happened. Splitting them
// across three addresses would make the middle one -- the policy -- look like
// a setting rather than the thing that connects the other two.
//
// AUTHORIZATION IS A COURTESY HERE, as everywhere else in the portal: every
// read behind this page is caller-scoped at the engine (the queries filter on
// `args.ownerUserId == actor.userId`), so this file decides what renders, not
// what is permitted.

// The engine's closed runnable set, in the order a person would pick from.
const SELECTABLE_APPS = ["claude-code", "codex"];

// The planner task kinds worth delegating. Coding and file production is what
// a local coding agent is for; the rest of the planner's kinds are engine
// work with nothing to gain from a laptop.
const DELEGATABLE_KINDS = ["runCommand", "fileProcessor", "callTool", "persistResult"];

export function MachinesPage(): ReactNode {
  const machines = useMachines();
  const sessions = useAppSessions();
  const policy = useDelegationPolicy();

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow="v1:worker:registration"
          title="Machines"
          blurb="The computers running MemQL Cockpit for you, and the local apps each one can run. An app becomes selectable only when the machine's own policy allows it AND it is signed in -- so this list says exactly what the router will and will not pick."
        />

        <Band title="Your machines" panel>
          {machines.loading ? (
            <Skeleton />
          ) : machines.error !== "" ? (
            <ErrorMessage>{machines.error}</ErrorMessage>
          ) : machines.machines.length === 0 ? (
            <EmptyState statement="No machines are registered to you. Run `memql-cockpit worker run` on a computer you own to register one." />
          ) : (
            <ul className="flex flex-col gap-3">
              {machines.machines.map((machine) => (
                <li key={machine.id} className="rounded-lg border border-subtle p-3">
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <span className="text-sm font-medium text-fg">{machine.name}</span>
                    <span className="text-xs text-fg-muted">
                      {machine.revokedAt !== "" ? "revoked" : `last seen ${machine.lastSeenAt || "never"}`}
                    </span>
                  </div>
                  <div className="mt-1 text-xs text-fg-muted">
                    {machine.version || "unknown version"}
                    {machine.buildTag === "" ? "" : ` · ${machine.buildTag}`}
                  </div>
                  <AppList apps={machine.apps} />
                </li>
              ))}
            </ul>
          )}
        </Band>

        <Band title="Delegation">
          <DelegationPolicyEditor state={policy} />
        </Band>

        <Band title="Delegated runs" panel>
          {sessions.loading ? (
            <Skeleton />
          ) : sessions.error !== "" ? (
            <ErrorMessage>{sessions.error}</ErrorMessage>
          ) : sessions.sessions.length === 0 ? (
            <EmptyState statement="No app session has run yet. One appears here the first time the planner hands a task to a local app." />
          ) : (
            <ul className="flex flex-col gap-2">
              {sessions.sessions.map((session) => (
                <SessionRow key={session.id} session={session} />
              ))}
            </ul>
          )}
        </Band>
      </section>
    </Container>
  );
}

// AppList renders one machine's apps. The badge says exactly what the ENGINE
// says: a machine that has the binary but is not signed in, or whose
// policy.yaml does not allow it, is shown as present and NOT selectable --
// rendering it as available would send somebody hunting a routing bug that
// is not there.
function AppList({ apps }: { apps: MachineApp[] }): ReactNode {
  if (apps.length === 0) {
    return (
      <p className="mt-2 text-xs text-fg-muted">
        This machine reports no local apps. A cockpit older than the app
        protocol reports none, and so does one that found neither app on PATH.
      </p>
    );
  }
  return (
    <ul className="mt-2 flex flex-col gap-1">
      {apps.map((app) => (
        <li key={app.id} className="flex flex-wrap items-center gap-2 text-xs">
          <span className="font-medium text-fg">{appLabel(app.id)}</span>
          <span className="text-fg-muted">{app.version || "version unknown"}</span>
          <Badge tone={app.runnable ? "ok" : "neutral"}>
            {app.runnable ? "selectable" : "not selectable"}
          </Badge>
          {app.runnable ? null : (
            <span className="text-fg-muted">
              {!app.allowed ? "not in the machine's apps.allow" : "not signed in"}
            </span>
          )}
          {app.subscription === "present" ? <Badge tone="ok">subscription</Badge> : null}
          {app.subscription === "unknown" ? (
            <span className="text-fg-muted">subscription unreported</span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function SessionRow({ session }: { session: AppSessionCard }): ReactNode {
  return (
    <li className="flex flex-wrap items-center justify-between gap-2 rounded border border-subtle px-3 py-2 text-xs">
      <span className="flex flex-wrap items-center gap-2">
        <Link className="font-medium text-accent hover:underline" to={sessionPath(session.id)}>
          {appLabel(session.app)}
        </Link>
        <Badge tone={statusTone(session.status)}>{session.status}</Badge>
        <Badge tone={session.billing === "subscription" ? "ok" : "neutral"}>{session.billing}</Badge>
      </span>
      <span className="text-fg-muted">
        {session.usage.known
          ? `${session.usage.inputTokens + session.usage.outputTokens} tokens`
          : "usage not reported"}
        {" · "}
        {session.startedAt}
      </span>
    </li>
  );
}

function statusTone(status: string): "ok" | "warn" | "danger" | "neutral" {
  switch (status) {
    case "ended":
      return "ok";
    case "failed":
      return "danger";
    case "cancelled":
      return "warn";
    default:
      return "neutral";
  }
}

// DelegationPolicyEditor. The form is the whole policy, saved as one write:
// a per-field save would let a person switch delegation ON while the app
// order was still empty, which is a state the planner reads as "enabled but
// nothing to pick" and reports as no_apps_configured.
function DelegationPolicyEditor({
  state,
}: {
  state: ReturnType<typeof useDelegationPolicy>;
}): ReactNode {
  const [draft, setDraft] = useState<DelegationPolicy>(state.policy);

  useEffect(() => {
    setDraft(state.policy);
  }, [state.policy]);

  if (state.loading) return <Skeleton />;
  if (state.error !== "") return <ErrorMessage>{state.error}</ErrorMessage>;

  const submit = (event: FormEvent): void => {
    event.preventDefault();
    void state.save(draft);
  };

  const toggleIn = (list: string[], value: string): string[] =>
    list.includes(value) ? list.filter((item) => item !== value) : [...list, value];

  return (
    <form className="flex flex-col gap-4" onSubmit={submit}>
      {state.found ? null : (
        <Callout tone="neutral" title="Delegation is off">
          You have not set a delegation policy. Until you do, nothing is
          delegated -- tasks run inside the cluster as they always have. This
          is the default on purpose: an agent running on your own computer
          should be something you turned on.
        </Callout>
      )}

      <label className="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          checked={draft.preferSubscriptionApps}
          onChange={(e) => setDraft({ ...draft, preferSubscriptionApps: e.target.checked })}
        />
        <span>
          <span className="font-medium text-fg">Delegate eligible tasks to my local apps</span>
          <span className="mt-0.5 block text-xs text-fg-muted">
            The master switch. With it off, nothing below applies. With it on,
            a task is delegated only when a machine with an allowed, signed-in
            app is online right now -- otherwise it runs in the cluster. A plan
            never waits for a laptop to wake up.
          </span>
        </span>
      </label>

      <Field label="Apps, in the order to try them">
        <div className="flex flex-wrap gap-3 text-sm">
          {SELECTABLE_APPS.map((appId) => (
            <label key={appId} className="flex items-center gap-1.5">
              <input
                type="checkbox"
                checked={draft.appOrder.includes(appId)}
                onChange={() => setDraft({ ...draft, appOrder: toggleIn(draft.appOrder, appId) })}
              />
              <span>{appLabel(appId)}</span>
            </label>
          ))}
        </div>
        <p className="mt-1 text-xs text-fg-muted">
          The first app on this list that a machine actually has wins. An app
          you do not list is never selected, even on a machine that has it --
          the list is how you say which of your subscriptions to spend.
        </p>
      </Field>

      <Field label="Task kinds that may be delegated">
        <div className="flex flex-wrap gap-3 text-sm">
          {DELEGATABLE_KINDS.map((kind) => (
            <label key={kind} className="flex items-center gap-1.5">
              <input
                type="checkbox"
                checked={draft.eligibleKinds.includes(kind)}
                onChange={() =>
                  setDraft({ ...draft, eligibleKinds: toggleIn(draft.eligibleKinds, kind) })
                }
              />
              <span>{kind}</span>
            </label>
          ))}
        </div>
        <p className="mt-1 text-xs text-fg-muted">
          Nothing is delegated by kind unless you list it here. Turning
          delegation on does not silently opt every kind of work in with it.
        </p>
      </Field>

      <Field label="Most sessions at once">
        <Select
          value={String(draft.maxConcurrentSessions)}
          onChange={(next) =>
            setDraft({ ...draft, maxConcurrentSessions: Number(next) || 1 })
          }
        >
          {[1, 2, 3, 4].map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </Select>
      </Field>

      <Field label="Workspace root on the machine">
        <TextInput
          value={draft.workspaceRoot}
          placeholder="/Users/you/memql-workspaces"
          onChange={(next) => setDraft({ ...draft, workspaceRoot: next })}
        />
        <p className="mt-1 text-xs text-fg-muted">
          Each run gets its own directory under this root. The cockpit refuses
          a workspace outside its own policy roots, so this is a preference the
          machine still gets to veto.
        </p>
      </Field>

      {state.saveError === "" ? null : <ErrorMessage>{state.saveError}</ErrorMessage>}

      <div>
        <Button type="submit" disabled={state.saving}>
          {state.saving ? "Saving..." : "Save delegation policy"}
        </Button>
      </div>
    </form>
  );
}
