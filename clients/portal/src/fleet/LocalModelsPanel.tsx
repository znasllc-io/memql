import type { ReactNode } from "react";

import { Badge, Button, Callout, DataText, EmptyState, Panel, StatusDot } from "../ui";
import {
  formatContextWindow,
  type FleetMachineRow,
  type FleetModelRow,
  summarizeFleet,
} from "./useFleetModels";

// The live model catalog, rendered (epic memql#4676, task memql#4683).
//
// ===========================================================================
// AN OFFLINE MODEL IS SHOWN, NOT HIDDEN
// ===========================================================================
// The catalog lists a model whose only machine is asleep, and so does this
// panel. The question an operator brings to this page is "why is my local
// model not being used", and a list that quietly dropped the answer would
// leave them with nothing to look at. So an asleep model appears, marked, with
// the machine that would serve it named.
//
// ===========================================================================
// "FULLY LOCAL" IS SAID OUT LOUD
// ===========================================================================
// It is the outcome the whole epic is for, and there is no other place an
// operator can confirm it. It is claimed only when a cloud provider is
// genuinely absent: saying it while a key sits configured would be a statement
// about spend that is not true.

function ModelCapabilities({ model }: { model: FleetModelRow }): ReactNode {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <Badge tone={model.structuredOutput ? "ok" : "neutral"}>
        {model.structuredOutput ? "structured output" : "no structured output"}
      </Badge>
      {model.embeddings ? <Badge tone="ok">embeddings</Badge> : null}
      <Badge tone="neutral">{formatContextWindow(model.contextWindow)}</Badge>
    </div>
  );
}

function MachineLine({ machine }: { machine: FleetMachineRow }): ReactNode {
  // BUSY IS AGAINST THE CEILING THE MACHINE DECLARED. A machine that declared
  // none is never busy -- it asked for no limit, so the limit is not the thing
  // to describe it by, and rendering "3 of 0" would be nonsense.
  const load =
    machine.maxConcurrent > 0
      ? `${machine.activeCount} of ${machine.maxConcurrent} in flight`
      : `${machine.activeCount} in flight, no declared limit`;
  return (
    <li className="flex flex-wrap items-center gap-2 py-1">
      <StatusDot tone={machine.online ? "ok" : "neutral"} label={machine.online ? "online" : "offline"} />
      <DataText kind="id">{machine.label}</DataText>
      {machine.runtimes.length > 0 ? (
        <span className="text-xs text-muted">{machine.runtimes.join(", ")}</span>
      ) : null}
      <span className="text-xs text-muted">{load}</span>
      {machine.busy ? <Badge tone="warn">busy</Badge> : null}
    </li>
  );
}

export function LocalModelsPanel({
  models,
  cloudConfigured,
  loading,
  error,
  onReload,
  onAddMachine,
}: {
  models: readonly FleetModelRow[];
  cloudConfigured: boolean;
  loading: boolean;
  error: string;
  onReload: () => void;
  onAddMachine?: () => void;
}): ReactNode {
  const summary = summarizeFleet(models, cloudConfigured);

  return (
    <Panel>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="text-base font-medium">Local models</h3>
          <p className="mt-1 text-sm text-muted">{summary.headline}</p>
        </div>
        <Button tone="quiet" size="sm" onClick={onReload} disabled={loading}>
          {loading ? "Reading…" : "Refresh"}
        </Button>
      </div>

      {error ? (
        <div className="mt-3">
          <Callout tone="danger" title="The model catalog could not be read">
            {error}
          </Callout>
        </div>
      ) : null}

      {summary.tone === "local" ? (
        <div className="mt-3">
          <Callout tone="ok" title="Running fully local">
            Planning, routing, suggestions and embeddings run on your own machines. Nothing on this
            cluster is billed per token.
          </Callout>
        </div>
      ) : null}

      {models.length === 0 && !loading ? (
        <div className="mt-4">
          <EmptyState
            statement="No machine in your fleet is offering a model."
            action={
              onAddMachine ? (
                <Button size="sm" onClick={onAddMachine}>
                  Pair a machine
                </Button>
              ) : undefined
            }
          />
        </div>
      ) : null}

      <ul className="mt-4 space-y-4">
        {models.map((model) => (
          <li key={model.modelId} className="border-t border-line pt-3 first:border-t-0 first:pt-0">
            <div className="flex flex-wrap items-center gap-2">
              <StatusDot tone={model.online ? "ok" : "neutral"} label={model.online ? "available" : "asleep"} />
              <DataText kind="id">{model.modelId}</DataText>
              <span className="text-xs text-muted">
                {model.onlineCount} of {model.machineCount}{" "}
                {model.machineCount === 1 ? "machine" : "machines"} online
              </span>
            </div>
            <div className="mt-2">
              <ModelCapabilities model={model} />
            </div>
            <ul className="mt-2">
              {model.machines.map((machine) => (
                <MachineLine key={machine.registrationId} machine={machine} />
              ))}
            </ul>
          </li>
        ))}
      </ul>
    </Panel>
  );
}
