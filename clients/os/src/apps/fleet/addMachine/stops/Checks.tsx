import { Caption, CopyField, Rail, type Stop } from "../../../../kit";
import { PullBar, SetupDownloadBar } from "../../machines/ModelDownloads";
import type { Check } from "../flow";

// CHECKS: online, steady, and what you asked for (design record D4, D5, D13,
// D14; memql#6214).
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
// A check with nothing to do has no body at all, and nothing here is a
// banner: a refused or failed download is the Models line, and the footer's
// Download models is the way to try again.

export function ChecksStop({ checks }: { checks: readonly Check[] }) {
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
    if (check.pull === undefined && check.download === undefined && check.repair === undefined && check.command === undefined) return undefined;
    return (
      <div className="os-stop-body os-fleet-repair">
        {check.pull === undefined ? null : <PullBar pull={check.pull} />}
        {check.download === undefined ? null : <SetupDownloadBar stage={check.download} />}
        {check.repair === undefined ? null : <Caption>{check.repair}</Caption>}
        {check.command === undefined ? null : (
          <CopyField value={check.command} label={`the ${check.name.toLowerCase()} command`} />
        )}
      </div>
    );
  }
}
