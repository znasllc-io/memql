import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import {
  CHECKLIST_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  TABLE_ELEMENT,
  TIMELINE_ELEMENT,
} from "@znasllc-io/memql-view-kit";
import type { DeployEnv } from "../deploy/useDeployConsole";

import {
  COMPONENT_CONCEPT,
  GATE_LEG_CONCEPT,
  ROLLOUT_CONCEPT,
  componentRows,
  gateLegRows,
  rolloutRows,
} from "../deploy/rows";
import { useDeployConsole } from "../deploy/useDeployConsole";
import { ErrorMessage } from "../components/StatusMessage";
import { ViewElement } from "./ViewElement";
import { Band, MetaButton, type ViewProps } from "./ViewLayout";

// Deployments: what is running, what shipped before it, and whether it is
// safe to ship again.
//
// LAYOUT RATIONALE. The only view built from TWO sources, and the bands are
// ordered by how urgent each is rather than by where it comes from. What is
// live right now (the gate, then the images) sits above the history, because
// an operator opening this page mid-incident needs "did the gate pass" before
// they need "what shipped in June". The history is a TIMELINE for the same
// reason the audit trail is: a deployment record is append-only and every
// status transition appends to one row's timeline, so the sequence IS the
// data.
//
// THE GATE IS A CHECKLIST, and that is the whole argument for adapting
// DeployControlService's payloads into row sets (src/deploy/rows.ts): a gate
// leg is a named thing that passed or did not, which is exactly what the
// checklist element renders, so it renders here through the same element,
// with the same theming, instead of a bespoke list of ticks.
//
// AUTHORIZATION. Three floors, all enforced server-side
// (component/deploycontrol/service.go) and all only HIDDEN here: view is
// admin+, cut and deploy are developer+, rollback is owner only. A caller
// below a floor sees the action absent rather than present-and-refused. The
// gate itself lives in src/deploy/useDeployConsole.ts, with a longer note on
// why a copy of the matrix in the UI is a courtesy and not a control.

export function DeploymentsView({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: ViewProps): ReactNode {
  // Staging first, always. Landing an operator on production is an invitation
  // to act on it by reflex.
  const [env, setEnv] = useState<DeployEnv>("staging");
  const console_ = useDeployConsole(env);

  const { permissions, status, loading, error, actionMessage, actionError, actionAuditEventId, busy } =
    console_;
  const selection = selectedRowId ? { selectedRowId } : {};
  const legs = gateLegRows(status);
  const images = componentRows(status);
  const rollouts = rolloutRows(status);

  return (
    <>
      <Band>
        <div className="flex flex-wrap items-center gap-3">
          <EnvPicker env={env} onChange={setEnv} />
          <ReleaseReading
            env={env}
            version={status?.version ?? ""}
            engineVersion={status?.engineVersion ?? ""}
            sync={status?.argocd.syncStatus ?? ""}
            health={status?.argocd.healthStatus ?? ""}
            outOfSync={status?.argocd.outOfSync === true}
            loading={loading}
            canView={permissions.canView}
          />
        </div>
        {/* Errors reuse the shared banner so a deploy failure reads exactly
            like every other failure in the portal. The success note is its
            mirror -- a tinted panel with full-contrast text, never coloured
            small text: --portal-ok on the page ground is about 3.4:1 in light
            mode, under the floor for 13px. */}
        {error ? (
          <div className="mt-3">
            <ErrorMessage>Could not read {env}: {error}</ErrorMessage>
          </div>
        ) : null}
        {actionError ? (
          <div className="mt-3">
            <ErrorMessage>
              {actionError}
              <RefusalAuditLink id={actionAuditEventId} />
            </ErrorMessage>
          </div>
        ) : null}
        {actionMessage ? (
          <p
            role="status"
            className="mt-3 rounded border border-ok bg-ok-subtle px-3 py-2 text-sm text-fg"
          >
            {actionMessage}
          </p>
        ) : null}
      </Band>

      {permissions.canShip || permissions.canRollBack ? (
        <Band
          title="Ship"
          meta={
            selectedRowId
              ? "Acting on the selected deployment"
              : "Select a deployment below to deploy or roll back to it"
          }
        >
          <div className="flex flex-wrap items-center gap-2">
            {permissions.canShip ? (
              <>
                <MetaButton onClick={() => console_.cut("patch")} disabled={busy}>
                  Cut a patch version
                </MetaButton>
                <MetaButton onClick={() => console_.cut("minor")} disabled={busy}>
                  Cut a minor version
                </MetaButton>
                <MetaButton
                  onClick={() => console_.ship(selectedRowId)}
                  disabled={busy || selectedRowId === ""}
                >
                  Deploy the selected version
                </MetaButton>
              </>
            ) : null}
            {permissions.canRollBack ? (
              <MetaButton
                tone="danger"
                onClick={() => console_.rollBack(selectedRowId)}
                disabled={busy || selectedRowId === ""}
              >
                Roll back to the selected version
              </MetaButton>
            ) : null}
          </div>
        </Band>
      ) : null}

      {permissions.canView ? (
        <Band
          title="Last gate"
          meta={
            status?.gateResult.ranAt
              ? `${status.gateResult.result || "unknown"}, ran ${status.gateResult.ranAt}`
              : "no gate result recorded"
          }
        >
          {legs.length === 0 ? (
            <p className="text-sm text-subtle">
              {loading ? "Reading the gate…" : `No gate has run against ${env} yet.`}
            </p>
          ) : (
            <ViewElement element={CHECKLIST_ELEMENT} rows={legs} concept={GATE_LEG_CONCEPT} />
          )}
        </Band>
      ) : (
        <Band title="Live state">
          <p className="text-sm text-subtle">
            Reading a deployment's live state needs the admin or owner role. Your
            role is {permissions.role || "not established on this connection"}. The
            deployment history below is what the cluster returns for you.
          </p>
        </Band>
      )}

      {permissions.canView && images.length > 0 ? (
        <Band title="Images in force" meta={`${env} overlay, resolved digests`} panel>
          <ViewElement element={TABLE_ELEMENT} rows={images} concept={COMPONENT_CONCEPT} />
        </Band>
      ) : null}

      {permissions.canView && rollouts.length > 0 ? (
        <Band title="Rollouts in flight" panel>
          <ViewElement element={TABLE_ELEMENT} rows={rollouts} concept={ROLLOUT_CONCEPT} />
        </Band>
      ) : null}

      <Band title="By outcome" meta="every deployment recorded, not just this environment">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          // The concept declares status="status"; the rail finds it through
          // the display card.
          options={{ bindings: { value: [] } }}
        />
      </Band>

      <Band title="History" meta="Newest first, by last status transition" panel>
        <ViewElement
          element={TIMELINE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{
            ...selection,
            bindings: {
              at: "updatedAt",
              label: "version",
              detail: "environment",
              status: "status",
            },
          }}
          onSelect={onSelect}
        />
      </Band>
    </>
  );
}

// The reference for a REFUSED action (memql#3334).
//
// A denial is an audited event, so the operator who was told "no" gets the
// same handle the operator who was told "yes" gets: an id to quote and a way
// into the trail. Until #3334 the deploy console had nothing to render here
// and said nothing rather than inventing an id -- correct behaviour for a
// surface that was not being given one, and exactly the gap the issue names.
//
// Renders NOTHING when the id is empty, which is the common case for a failure
// that was not a refusal: an invalid argument is rejected before the gate, and
// an UNAVAILABLE never reached the identity node, so neither wrote an event.
// Showing "audited as (none)" there would be worse than silence.
//
// Deliberately the same shape as the admin console's AuditLink
// (src/admin/WriteOutcome.tsx) -- one convention for "here is the row that
// records what just happened", across both gated write surfaces.
function RefusalAuditLink({ id }: { id: string }): ReactNode {
  if (id === "") return null;
  return (
    <>
      {" "}
      <span className="text-xs text-muted">
        Audited as <span className="font-mono break-all">{id}</span> —{" "}
        <Link to="/views/audit" className="underline hover:text-fg">
          open the trail
        </Link>
      </span>
    </>
  );
}

// The environment picker. Two literal buttons rather than a loop: there are
// exactly two deploy targets, the server validates the set, and writing them
// out keeps this module free of any iteration over data.
function EnvPicker({
  env,
  onChange,
}: {
  env: DeployEnv;
  onChange: (next: DeployEnv) => void;
}): ReactNode {
  return (
    <div
      role="group"
      aria-label="Environment"
      className="flex overflow-hidden rounded border border-line"
    >
      <EnvButton value="staging" env={env} onChange={onChange} />
      <EnvButton value="prod" env={env} onChange={onChange} />
    </div>
  );
}

function EnvButton({
  value,
  env,
  onChange,
}: {
  value: DeployEnv;
  env: DeployEnv;
  onChange: (next: DeployEnv) => void;
}): ReactNode {
  const active = env === value;
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={() => onChange(value)}
      className={
        "px-3 py-1 text-xs font-medium " +
        (active ? "bg-accent text-accent-fg" : "bg-surface text-muted hover:bg-raised")
      }
    >
      {value === "prod" ? "Production" : "Staging"}
    </button>
  );
}

// The headline reading. Deliberately NOT stat tiles: a release is one record,
// not a population, and the four facts on it are strings -- a version, an
// engine version, a sync state and a health state. Tiles would render "1
// deployment" and tell an operator nothing.
function ReleaseReading({
  env,
  version,
  engineVersion,
  sync,
  health,
  outOfSync,
  loading,
  canView,
}: {
  env: DeployEnv;
  version: string;
  engineVersion: string;
  sync: string;
  health: string;
  outOfSync: boolean;
  loading: boolean;
  canView: boolean;
}): ReactNode {
  if (!canView) {
    return <p className="text-sm text-subtle">Live state is not visible to your role.</p>;
  }
  if (loading && version === "") {
    return <p className="text-sm text-muted">Reading {env}…</p>;
  }
  if (version === "") {
    return <p className="text-sm text-subtle">Nothing is pinned in the {env} overlay.</p>;
  }
  return (
    <p className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
      <span className="text-lg font-semibold tracking-tight">{version}</span>
      {engineVersion && engineVersion !== version ? (
        <span className="text-muted">engine {engineVersion}</span>
      ) : null}
      {/* Drift is said in words, at full contrast, rather than in a hue:
          --portal-warn is a ~2.3:1 amber in light mode and would be the least
          legible thing on the page while carrying the most weight. */}
      {sync ? (
        <span className={outOfSync ? "font-medium text-fg" : "text-muted"}>
          {sync}
          {outOfSync ? " — the cluster does not match the overlay" : ""}
        </span>
      ) : null}
      {health ? <span className="text-muted">{health}</span> : null}
    </p>
  );
}
