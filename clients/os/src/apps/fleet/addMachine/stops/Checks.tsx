import { Button, Caption, CopyField, Notice, Rail, type Stop } from "../../../../kit";
import { machineName, type MachineRow } from "../../rows";
import type { Check } from "../flow";

// CHECKS: online, steady, and what you asked for (design record D4, D5, D13,
// D14).
//
// ===========================================================================
// A RAIL INSIDE A STOP, NOT A SECOND VOCABULARY
// ===========================================================================
// Each check is a mark, a name and its answer -- which is exactly what a rail
// stop is -- so the checks draw as a short rail of their own beneath the
// Checks stop, with the same marks, the same colours and the same states the
// page's rail uses. A list with its own dots would be a second way of saying
// "done", "moving", "yours", and a person reading down the page would have to
// learn it.
//
// No `openStop`: every check renders its body, because a check's body is its
// repair, and a repair folded behind a chevron is a repair somebody misses.
// Settled checks with nothing to offer have no body at all.

export function ChecksStop({
  checks,
  machine,
  pulling,
  pullError,
  onPullRecommended,
  onRetryResponse,
}: {
  checks: readonly Check[];
  machine: MachineRow;
  /** Whether the recommended pull has been asked for and not yet answered. */
  pulling: boolean;
  /** The cluster's refusal of the pull, verbatim, or "". */
  pullError: string;
  onPullRecommended: () => void;
  onRetryResponse?: () => void;
}) {
  const label = machineName(machine);

  const stops: Stop[] = checks.map((check) => ({
    id: check.id,
    name: check.name,
    state: check.state,
    sentence: check.answer,
    body: bodyFor(check),
  }));

  return (
    <div className="os-fleet-checks">
      <Rail stops={stops} label="Checks on this machine" />
    </div>
  );

  function bodyFor(check: Check) {
    const hasRepair = check.repair !== undefined;
    const hasAct = check.act !== undefined;
    const hasPullError = check.id === "models" && pullError !== "";
    if (!hasRepair && !hasAct && !hasPullError && !check.details) return undefined;
    return (
      <div className="os-stop-body os-fleet-repair">
        {check.details ? <Rail label="Permission results" stops={check.details.map(d => ({ id: d.name, name: d.name, state: d.state, sentence: d.answer }))} /> : null}
        {check.act === "pullRecommended" ? (
          <div className="os-fleet-repair-act">
            <Button tone="primary" busy={pulling} busyLabel="Asking the machine..." onClick={onPullRecommended}>
              Pull the recommended models
            </Button>
            <Caption>Onto {label}, in the order the catalog recommends for its class. Several gigabytes.</Caption>
          </div>
        ) : null}
        {hasPullError ? (
          <Notice
            tone="error"
            sentence="The recommended pulls did not all start."
            next="Some models may already be downloading. Check their progress before retrying; the cluster's reason is below."
            detail={pullError}
          />
        ) : null}
        {check.repair === undefined ? null : <Caption>{check.repair}</Caption>}
        {check.command === undefined ? null : (
          <CopyField value={check.command} label={`the ${check.name.toLowerCase()} command`} />
        )}
        {check.act === "retryResponse" ? <Button onClick={onRetryResponse}>Retry check</Button> : null}
      </div>
    );
  }
}
