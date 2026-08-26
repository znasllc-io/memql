import type { ReactNode } from "react";

import { Badge, Button, DataText, ErrorNotice, Panel } from "../ui";

// The `no_local_model_available` park (epic memql#4676, task memql#4683).
//
// ===========================================================================
// THE MACHINE LIST IS THE POINT, NOT DECORATION
// ===========================================================================
// A card saying only "no local model available" tells a person the router
// found nothing. The question they actually have is which of their four
// machines was ruled out and why -- and this is the surface where that gets
// answered, out of the report the refusal carries.
//
// ===========================================================================
// APPROVE-CLOUD IS ABSENT, NOT DISABLED, WHEN THERE IS NO CLOUD PROVIDER
// ===========================================================================
// A disabled button advertises a capability whose only explanation is a
// refusal. Worse here: it converts "your machines are asleep" into "you
// clicked the fix and it did not fix it", which is much harder to act on than
// the original problem.

export interface RuledOutMachine {
  machine: string;
  reason: string;
}

export interface NoLocalModelPark {
  model: string;
  machinesTotal: number;
  machinesRuledOut: RuledOutMachine[];
  cloudProviderConfigured: boolean;
  lastError?: string;
}

type Bag = Record<string, unknown>;

// parsePark decodes the feedbackRequest payload the planner stamped. A payload
// it cannot read yields null rather than a half-built card: a park card
// missing the very list it exists to show would leave the reader worse off
// than the plain status line.
export function parsePark(payload: unknown): NoLocalModelPark | null {
  if (!payload || typeof payload !== "object") return null;
  const bag = payload as Bag;
  if (bag.code !== "no_local_model_available") return null;
  const ruledOut = Array.isArray(bag.machinesRuledOut)
    ? (bag.machinesRuledOut as Bag[])
        .map((entry) => ({
          machine: typeof entry.machine === "string" ? entry.machine : "",
          reason: typeof entry.reason === "string" ? entry.reason : "",
        }))
        .filter((entry) => entry.machine !== "")
    : [];
  return {
    model: typeof bag.model === "string" ? bag.model : "",
    machinesTotal: typeof bag.machinesTotal === "number" ? bag.machinesTotal : 0,
    machinesRuledOut: ruledOut,
    cloudProviderConfigured: bag.cloudProviderConfigured === true,
    lastError: typeof bag.lastError === "string" ? bag.lastError : undefined,
  };
}

export function NoLocalModelCard({
  park,
  busy,
  onWakeMachine,
  onApproveCloud,
}: {
  park: NoLocalModelPark;
  busy?: boolean;
  onWakeMachine?: () => void;
  onApproveCloud?: () => void;
}): ReactNode {
  // "You have no machines" and "none of your four matched" are different
  // problems with different fixes, so the card says which.
  const headline =
    park.machinesTotal === 0
      ? `This plan needs ${park.model || "a local model"}, and no machine is paired to this account yet.`
      : `No machine in your fleet can run ${park.model || "the model this plan needs"} right now.`;

  return (
    <Panel>
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone="warn">waiting on you</Badge>
        <h3 className="text-base font-medium">Paused: no local model available</h3>
      </div>

      <p className="mt-2 text-sm">{headline}</p>
      <p className="mt-1 text-sm text-muted">
        This plan&rsquo;s policy names no cloud fallback, so it is waiting rather than running on a
        paid provider.
      </p>

      {park.machinesRuledOut.length > 0 ? (
        <ul className="mt-3 space-y-1">
          {park.machinesRuledOut.map((entry) => (
            <li key={entry.machine} className="flex flex-wrap items-baseline gap-2 text-sm">
              <DataText kind="id">{entry.machine}</DataText>
              <span className="text-muted">{entry.reason}</span>
            </li>
          ))}
        </ul>
      ) : null}

      {park.lastError ? (
        <div className="mt-3">
          {/* The last machine actually attempted failed with something. That
              is a RAW string from a runtime, so it goes behind ErrorNotice's
              owner/admin disclosure (memql#4653) rather than into the card a
              person reads. The sentence comes from what the call was trying
              to do, which this call site knows and the error text does not. */}
          <ErrorNotice
            sentence="The last machine that was tried did not finish the call."
            next="Waking another machine, or approving cloud for this plan, will let it continue."
            detail={park.lastError}
          />
        </div>
      ) : null}

      <div className="mt-4 flex flex-wrap gap-2">
        {onWakeMachine ? (
          <Button size="sm" onClick={onWakeMachine} disabled={busy}>
            Wake a machine
          </Button>
        ) : null}
        {park.cloudProviderConfigured && onApproveCloud ? (
          <Button tone="quiet" size="sm" onClick={onApproveCloud} disabled={busy}>
            Run this plan on the cloud instead
          </Button>
        ) : null}
      </div>
    </Panel>
  );
}
