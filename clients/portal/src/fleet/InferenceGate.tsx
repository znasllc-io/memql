import type { ReactNode } from "react";

import { Badge, Button, Callout, DataText, Panel } from "../ui";
import type { InferenceStatus } from "./useInferenceStatus";

// The inference step of the first-run gate (epic memql#4676, task memql#4684,
// design G).
//
// ===========================================================================
// THE REQUIREMENT LIVES HERE, NOT IN THE ENGINE (D7 / D8)
// ===========================================================================
// MemQL is an intelligent system, so USING it requires inference -- but
// STARTING it does not. The engine boots, serves and migrates with no provider
// configured, and inference-needing features already refuse or park typed. So
// the requirement is RENDERED, never enforced server-side, and nothing in this
// component's absence would stop a cluster from running.
//
// ===========================================================================
// LOCAL LEADS, AND THE ORDER IS THE RECOMMENDATION
// ===========================================================================
// The three doors are not equivalent. A model on a machine the person already
// owns costs nothing per token and sends no prompt to a vendor; a key is a
// credential at rest and a bill. Listing them side by side would describe a
// neutrality this product does not have.
//
// ===========================================================================
// A DOOR ALREADY OPEN PASSES SILENTLY
// ===========================================================================
// If federation is already configured, this step never renders -- the caller
// checks `status.eligible` first. What renders here is only ever the state
// where a person genuinely has to choose something.

function DoorCard({
  title,
  recommended,
  body,
  action,
}: {
  title: string;
  recommended?: boolean;
  body: ReactNode;
  action?: ReactNode;
}): ReactNode {
  return (
    <li className="rounded-md border border-line p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-medium">{title}</h3>
        {recommended ? <Badge tone="ok">recommended</Badge> : null}
      </div>
      <div className="mt-1 text-sm text-muted">{body}</div>
      {action ? <div className="mt-3">{action}</div> : null}
    </li>
  );
}

export function InferenceGate({
  status,
  loading,
  error,
  canSkip,
  onPairMachine,
  onEnterKey,
  onRecheck,
  onSkip,
}: {
  status: InferenceStatus | null;
  loading: boolean;
  error: string;
  // canSkip is true in EXACTLY ONE mode: an auth-disabled cluster, where
  // there is no identity and the operator is troubleshooting. A gate that
  // could not be passed in that mode would lock somebody out of the surface
  // they were troubleshooting with.
  canSkip: boolean;
  onPairMachine?: () => void;
  onEnterKey?: () => void;
  onRecheck?: () => void;
  onSkip?: () => void;
}): ReactNode {
  const contextFloor = status?.minimumContextWindow ?? 0;

  return (
    <Panel>
      <h2 className="text-lg font-medium">Choose where this cluster thinks</h2>
      <p className="mt-1 text-sm text-muted">
        MemQL runs planning, routing and suggestions through a model. Pick one source and you are
        done &mdash; you can change it later.
      </p>

      {error ? (
        <div className="mt-3">
          <Callout tone="danger" title="Could not read this cluster's inference status">
            {error}
          </Callout>
        </div>
      ) : null}

      {status && !status.fleetInferenceInstalled ? (
        <div className="mt-3">
          {/* "Your machines are asleep" and "this node has no worker service"
              are identical from a page and have entirely different fixes, so
              the second one says so rather than being folded into the first. */}
          <Callout tone="warn" title="This node cannot reach your machines">
            The node serving this page has no worker service, so local models are not an option from
            here even if your machines are awake. An API key or the Anthropic federation will work.
          </Callout>
        </div>
      ) : null}

      <ul className="mt-4 space-y-3">
        <DoorCard
          title="Run a model on your own machine"
          recommended
          body={
            status && status.localModelCount > 0 ? (
              <>
                Your fleet reports {status.localModelCount}{" "}
                {status.localModelCount === 1 ? "model" : "models"}, but{" "}
                {status.localModelCount === 1 ? "it does" : "none does"} not meet what the platform
                needs: structured output and at least{" "}
                {contextFloor > 0 ? `${Math.round(contextFloor / 1000)}k` : "the minimum"} context.
                A 7&ndash;8B instruct model such as llama3.1:8b or qwen2.5:7b does.
              </>
            ) : (
              <>
                Pair a machine that runs Ollama and MemQL will use its models. Nothing is billed per
                token, and no prompt leaves your hardware. The floor is a 7&ndash;8B instruct model
                with structured output.
              </>
            )
          }
          action={
            onPairMachine ? (
              <Button size="sm" onClick={onPairMachine}>
                Pair a machine
              </Button>
            ) : undefined
          }
        />

        <DoorCard
          title="Use the Anthropic workload-identity federation"
          body={
            <>
              No key at rest anywhere: each pod exchanges its own Kubernetes token for a short-lived
              bearer. Configured outside the portal &mdash; once it is complete, this step passes on
              its own.
            </>
          }
          action={
            onRecheck ? (
              <Button tone="quiet" size="sm" onClick={onRecheck} disabled={loading}>
                {loading ? "Checking…" : "Check again"}
              </Button>
            ) : undefined
          }
        />

        <DoorCard
          title="Enter an API key"
          body={
            <>
              An Anthropic or OpenAI key, stored the way this cluster stores every provider
              credential. Calls are billed to that account.
            </>
          }
          action={
            onEnterKey ? (
              <Button tone="quiet" size="sm" onClick={onEnterKey}>
                Add a key
              </Button>
            ) : undefined
          }
        />
      </ul>

      {status && status.eligibleModelIds.length > 0 ? (
        <p className="mt-4 text-sm text-muted">
          Ready to use: <DataText kind="id">{status.eligibleModelIds.join(", ")}</DataText>
        </p>
      ) : null}

      {canSkip && onSkip ? (
        <div className="mt-4 border-t border-line pt-3">
          <p className="text-xs text-muted">
            This cluster has authentication disabled, so you are not really signed in. You can
            continue without configuring inference &mdash; features that need a model will refuse
            and say why.
          </p>
          <div className="mt-2">
            <Button tone="quiet" size="sm" onClick={onSkip}>
              Continue without inference
            </Button>
          </div>
        </div>
      ) : null}
    </Panel>
  );
}

// InferenceNotice is the RAIL-LEVEL message for a cluster that became
// ineligible AFTER someone was already using it -- the machine went to sleep.
//
// IT IS A NOTICE, NEVER AN EVICTION. Ejecting somebody mid-session because a
// laptop closed would punish them for a condition that fixes itself when they
// open it, and would take away the page they would use to find that out.
export function InferenceNotice({ status }: { status: InferenceStatus | null }): ReactNode {
  if (!status || status.eligible) return null;
  return (
    <Callout tone="warn" title="No model is available right now">
      {status.localModelCount > 0
        ? "The machines that host this cluster's models are asleep. Work that needs a model is paused, not lost, and resumes when one wakes up."
        : "No inference source is configured. Work that needs a model will pause and say so."}
    </Callout>
  );
}
