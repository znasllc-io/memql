import { useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";
import type { ModuleDetail, ModuleEnvVar } from "@znasllc-io/memql-sdk-core/client";

import {
  Badge,
  Band,
  Breadcrumbs,
  Button,
  ConfirmDialog,
  Container,
  DataText,
  ErrorNotice,
  Field,
  FormActions,
  FormRow,
  PageHeader,
  Skeleton,
  StatusDot,
  TextInput,
} from "../ui";
import { useAdminAccess } from "../admin/useAdminConsole";
import { ModulesRefused } from "./ModulesRefused";
import { stateTone, useModuleDetail, useSetPackEnabled } from "./useModules";
import { ObservabilitySection } from "./ObservabilitySection";
import { MODULES_ROOT } from "./urls";

// One module's detail (memql#4191): the environment-variable surface the
// envregistry manifest declares for it, evaluated on the answering node,
// plus the control the module's KIND actually has.
//
// SECRETS NEVER CARRY A VALUE. Not masked, not prefixed -- the engine sends
// set/unset and nothing else, there is no reveal call, and this page states
// that instead of drawing dots that imply a value is one click away.
export function ModuleDetailPage(): ReactNode {
  const params = useParams();
  const kind = params["kind"] ?? "";
  const name = params["name"] ?? "";
  const { role, canAdminister, resolved } = useAdminAccess();
  const isOwner = role === "owner";
  const state = useModuleDetail(kind, name, canAdminister);

  if (!canAdminister) {
    return <ModulesRefused role={role} resolved={resolved} />;
  }

  const detail = state.detail;
  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={
            <Breadcrumbs items={[{ label: "Modules", to: MODULES_ROOT }, { label: name }]} />
          }
          title={name}
          blurb={
            detail
              ? detail.module.description || `A ${detail.module.kind} module of this cluster.`
              : `A ${kind} module of this cluster.`
          }
          actions={
            <Button size="xs" onClick={state.reload} busy={state.loading} busyLabel="Reading…">
              Refresh
            </Button>
          }
          meta={
            detail ? (
              <span className="text-xs text-subtle">
                answered by <DataText kind="id">{detail.reportingNodeId || "unknown"}</DataText>
              </span>
            ) : undefined
          }
        />

        {state.error !== "" ? (
          <ErrorNotice sentence="Could not read this module." detail={state.error} />
        ) : detail === null ? (
          <Skeleton variant="kv" rows={6} />
        ) : (
          <>
            <Band title="State">
              <div className="flex flex-wrap items-center gap-3">
                <Badge tone={stateTone(detail.module.state)}>
                  {detail.module.state || "unknown"}
                </Badge>
                <span className="text-sm text-muted">
                  {detail.module.stateDetail || "Nothing unusual to report."}
                </span>
              </div>
              <p className="mt-2 text-xs text-subtle">
                {detail.module.scope === "cluster"
                  ? "Cluster-wide: read from the shared graph, the same on every node."
                  : "Node-scope: this is the answering binary's own fact; a different node type may answer differently."}
              </p>
            </Band>

            <ModuleControl detail={detail} isOwner={isOwner} onChanged={state.reload} />

            <Band
              title="Environment"
              meta={
                detail.envVars.length === 0
                  ? undefined
                  : `${detail.envVars.length} declared, evaluated on the answering node`
              }
              panel
            >
              {detail.envVars.length === 0 ? (
                <p className="p-3 text-sm text-muted">
                  The manifest declares no environment variables for this module.
                </p>
              ) : (
                <ul className="divide-y divide-line">
                  {detail.envVars.map((envVar) => (
                    <EnvVarRow key={envVar.name} envVar={envVar} />
                  ))}
                </ul>
              )}
            </Band>

            <ObservabilitySection module={detail.module} />
          </>
        )}
      </section>
    </Container>
  );
}

function EnvVarRow({ envVar }: { envVar: ModuleEnvVar }): ReactNode {
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-3 py-2">
      <span className="flex items-center gap-2">
        <StatusDot tone={envVar.set ? "ok" : "neutral"} label={envVar.set ? "set" : "unset"} />
        <DataText kind="id">{envVar.name}</DataText>
      </span>
      {envVar.secret ? (
        <span className="text-xs text-subtle">secret — the value never leaves the engine</span>
      ) : envVar.set ? (
        <DataText kind="string" title="resolved on the answering node">
          {envVar.value === "" ? '""' : envVar.value}
        </DataText>
      ) : (
        <span className="text-xs text-subtle">unset</span>
      )}
      {envVar.defaultValue !== "" && !envVar.secret ? (
        <span className="text-xs text-subtle">
          default <DataText kind="string">{envVar.defaultValue}</DataText>
        </span>
      ) : null}
      <span className="min-w-0 flex-1 basis-full truncate text-xs text-muted sm:basis-auto">
        {envVar.description}
      </span>
    </li>
  );
}

// The kind-appropriate control. Only a pack has a stored switch; the other
// kinds get an honest statement of what governs them instead of a disabled
// control that implies one is coming.
function ModuleControl({
  detail,
  isOwner,
  onChanged,
}: {
  detail: ModuleDetail;
  isOwner: boolean;
  onChanged: () => void;
}): ReactNode {
  if (detail.module.kind === "pack") {
    return <PackToggle detail={detail} isOwner={isOwner} onChanged={onChanged} />;
  }
  if (detail.module.kind === "integration") {
    return (
      <Band title="Enablement">
        <p className="text-sm text-muted">
          An integration's state is derived from its configuration -- credentials present means
          active, absent means it opts out at boot. There is no stored switch to flip; change the
          environment it reads (listed below) and restart the node.
        </p>
      </Band>
    );
  }
  if (detail.module.kind === "node-type") {
    return (
      <Band title="Enablement">
        <p className="text-sm text-muted">
          A node type's switch is replica scale, owned by the deployment record. A lane at zero
          replicas is deliberately off; the voice lane additionally holds itself at zero while its
          credentials are absent.
        </p>
      </Band>
    );
  }
  return null;
}

function PackToggle({
  detail,
  isOwner,
  onChanged,
}: {
  detail: ModuleDetail;
  isOwner: boolean;
  onChanged: () => void;
}): ReactNode {
  const flip = useSetPackEnabled();
  const [confirming, setConfirming] = useState(false);
  const [reason, setReason] = useState("");
  const enabled = detail.module.state === "enabled";
  const nextEnabled = !enabled;

  return (
    <Band title="Enablement">
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted">
          {enabled
            ? "Enabled: every node loads this pack's behavior at boot."
            : "Disabled: the pack is mounted inert -- its concepts and rows stay readable, its tools, queries and automations are absent."}
        </p>
        {flip.error !== "" ? <ErrorNotice sentence="This module was not enabled or disabled." next="It is still in the state shown above; try again." detail={flip.error} /> : null}
        {flip.outcome ? (
          <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            Saved: {flip.outcome.packDomain} is now{" "}
            {flip.outcome.enabled ? "enabled" : "disabled"}.
            {flip.outcome.restartRequired
              ? " Takes effect as each node restarts -- a running node keeps what it loaded at boot."
              : ""}
          </p>
        ) : null}
        {isOwner ? (
          <FormRow>
            <Field label="Reason" hint="Recorded with the flip; shown here and in the audit trail.">
              <TextInput value={reason} onChange={setReason} placeholder="Why the change" />
            </Field>
            <FormActions>
              <Button
                tone={nextEnabled ? "primary" : "danger"}
                onClick={() => setConfirming(true)}
                disabled={flip.busy}
              >
                {nextEnabled ? "Enable this pack" : "Disable this pack"}
              </Button>
            </FormActions>
          </FormRow>
        ) : (
          <p className="text-xs text-subtle">Flipping a pack is an owner-only action.</p>
        )}
      </div>

      <ConfirmDialog
        open={confirming}
        title={nextEnabled ? "Enable this pack?" : "Disable this pack?"}
        confirmLabel={nextEnabled ? "Enable the pack" : "Disable the pack"}
        tone={nextEnabled ? "primary" : "danger"}
        busy={flip.busy}
        onConfirm={() => {
          flip.flip(detail.module.name, nextEnabled, reason.trim(), () => {
            setConfirming(false);
            onChanged();
          });
        }}
        onCancel={() => setConfirming(false)}
      >
        {nextEnabled ? (
          <>
            The setting is saved to the shared graph now, and each node picks it up at its next
            restart -- this is not a live toggle. Once a node restarts, the pack's tools, queries
            and automations are registered again.
          </>
        ) : (
          <>
            The setting is saved to the shared graph now, and each node picks it up at its next
            restart -- this is not a live toggle. A restarted node keeps the pack's concepts and
            existing rows readable, but its tools, queries, mutations and automations will be
            absent until re-enabled.
          </>
        )}
      </ConfirmDialog>
    </Band>
  );
}
