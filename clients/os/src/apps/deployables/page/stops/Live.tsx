import { useCallback, useEffect, useState } from "react";
import { History, Undo2 } from "lucide-react";

import { Button, Caption, Chip, Fact, Facts } from "../../../../kit";
import { formatMoment } from "../../../../kit/format";
import { useOsConnection } from "../../../../live/connection";
import type { SiteLifecycleActions } from "../../packages/actions";
import { RuntimeSettingsPanel } from "./RuntimeSettings";
import { TrafficPanel } from "./Traffic";
import { fetchSiteVersions, MAX_HISTORY_VERSIONS, type SiteVersion } from "../../packages/calls";
import { ProblemNotice } from "../../packages/ReportView";
import type { SiteRow } from "../../rows";
import { isPublished, type RailProblem } from "../rail";

// The Live stop: what the address is serving, since when, and the acts that
// change it -- the deployable's lifecycle, moved here from the panel that
// carried it (epic memql#4794, D13) and read against the stop's own note.
//
// ===========================================================================
// SYSTEM-OWNED ROWS RENDER NO CONTROLS AT ALL
// ===========================================================================
// Not disabled controls -- NONE. The seeded portal and OS sites are exempt
// from the lifecycle entirely, and the server refuses a status write on them
// whoever asks. A row of greyed-out buttons would be six controls a person has
// to read past to learn they are not for them, which is the same rule the
// shell states about right-click: nothing happens where nothing is offered.
//
// The presentation is the courtesy; the guard beside the engine's write path
// is the gate.
//
// ===========================================================================
// AFTER A FIRST PUBLISH THE STOP SAYS ONE THING, AND THE HEAD DOES THE REST
// ===========================================================================
// "Published to <host>. Not serving yet." is the rail's note, and Make it live
// is the Head's action -- so a draft offers no second Make-it-live here, and
// no version list either: nothing is serving to roll back from. Archive stays,
// because a draft somebody is done with is filed the same way as anything
// else that is not serving.
//
// ===========================================================================
// THE VERSION LIST IS THE ROW'S OWN HISTORY, WALKED
// ===========================================================================
// There is no all-versions query, because "the graph's own history is the
// version list" -- so this re-issues siteById under successive `asOf`
// timestamps, and each entry it finds is a bundleRef that can be pointed back
// at. Loaded on DEMAND rather than with the stop: each version is a round
// trip, and most people opening a deployable are not rolling it back.
//
// ===========================================================================
// RAW bundleRef POINTING STAYS OUT
// ===========================================================================
// Deliberately (D13). Parity covers the four features somebody uses -- history,
// rollback, pause and resume, archive -- and not the operator escape hatch. A
// text field that accepts any URI is a way to point a live site at nothing.

// ONE NOTE FOR EVERY ABSENCE ON THIS STOP, not one per missing control.
// Three things are not here for a system-owned row -- the lifecycle, the
// runtime settings and the traffic figure -- for two reasons worth saying
// once: the row is re-seeded at boot, so a value set on it would be reverted;
// and the request log excludes these surfaces by their own row field, because
// measuring the console somebody reads a figure in would be measuring the act
// of looking. An absent control with no account of itself reads as something
// nobody built, which is the rule the Domains panel's missing re-check button
// already follows.
const SYSTEM_OWNED_NOTE =
  "This is one of the cluster's own surfaces. It is re-seeded live at every boot, so it has no lifecycle to change and no settings to give it -- and its traffic is not recorded, because measuring the console somebody reads a figure in would be measuring the act of looking.";

export function LiveStop({
  site,
  canWrite,
  lifecycle,
  refusal,
}: {
  site: SiteRow;
  canWrite: boolean;
  /**
   * The page's own status hook, shared with the Head's Make it live so that
   * a refused status write renders HERE, beside the other status controls,
   * whichever button asked for it.
   */
  lifecycle: SiteLifecycleActions;
  /** The newest run's refusal, when publishing is where it stopped. */
  refusal: RailProblem | null;
}) {
  const connection = useOsConnection();
  const [versions, setVersions] = useState<SiteVersion[] | null>(null);
  const [loadingVersions, setLoadingVersions] = useState(false);

  const loadVersions = useCallback(async () => {
    if (connection === null) return;
    setLoadingVersions(true);
    try {
      setVersions(await fetchSiteVersions(connection.query, site.id));
    } catch {
      // A history that could not be read is an absent history, not a broken
      // stop: the rest of this surface still works and the walk can be
      // retried. The write path reports its own refusals, verbatim.
      setVersions([]);
    } finally {
      setLoadingVersions(false);
    }
  }, [connection, site.id]);

  // Re-close the history when the selection changes, so a list loaded for one
  // deployable is never shown under another.
  useEffect(() => {
    setVersions(null);
  }, [site.id]);

  if (site.systemOwned) {
    return (
      <div className="os-stop-body">
        <Caption>{SYSTEM_OWNED_NOTE}</Caption>
      </div>
    );
  }

  const live = site.status === "live";
  const paused = site.status === "disabled";
  const serving = live || paused;

  // THE READINGS ARE A READ, so they are not behind the write gate. Somebody
  // who owns a deployable but does not hold the admin rung the lifecycle
  // controls need can still see whether anybody is using their own app, and
  // what it is configured with. Before the readings existed this stop had
  // nothing to show such a reader and returned null, which is why the
  // condition grew a term rather than being replaced.
  const hasReadings = site.status !== "draft";
  if (refusal === null && !live && !canWrite && !hasReadings) return null;

  return (
    <div className="os-stop-body">
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}

      {live ? (
        <Facts>
          <Fact label="Live since" value={formatMoment(site.createdAt)} />
        </Facts>
      ) : null}

      {/* IS ANYBODY USING IT, AND IS IT HEALTHY (epic memql#4906) -- above the
          history and the acts that change it, because it is what somebody
          opening a live deployable came to find out. */}
      <TrafficPanel site={site} />

      {canWrite && serving && isPublished(site) ? (
        <section className="os-report-part">
          <h4 className="os-report-heading">
            <History size={12} aria-hidden /> Versions
          </h4>
          {versions === null ? (
            <>
              <Caption>Each version is a point in this deployable's own history. Loading them is a few reads.</Caption>
              <Button onClick={() => void loadVersions()} busy={loadingVersions} busyLabel="Reading history">
                <History size={12} aria-hidden /> Show the last {MAX_HISTORY_VERSIONS}
              </Button>
            </>
          ) : versions.length === 0 ? (
            <Caption>No earlier versions. This deployable has been published once.</Caption>
          ) : (
            <ul className="os-versions">
              {versions.map((v, i) => (
                <li key={`${v.createdAt}-${v.bundleRef}`} className="os-version" data-current={i === 0 ? "true" : "false"}>
                  <span className="os-version-when">{formatMoment(v.createdAt)}</span>
                  <span className="os-version-ref os-mono">{versionLabel(v.bundleRef)}</span>
                  {i === 0 ? (
                    <Chip tone="accent">serving now</Chip>
                  ) : (
                    <Button
                      onClick={() => void lifecycle.rollTo(site.id, v.bundleRef)}
                      busy={lifecycle.busy}
                      ariaLabel={`Roll ${site.hostname} back to the version from ${formatMoment(v.createdAt)}`}
                    >
                      <Undo2 size={12} aria-hidden /> Roll back to this
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </section>
      ) : null}

      {/* What the app reads at load. Beneath the history because it is
          configuration rather than status, and above Availability because it
          is a smaller act than pausing. */}
      <RuntimeSettingsPanel site={site} canWrite={canWrite} />

      {/* THE LIFECYCLE ACTS ARE NOT HERE ANY MORE (epic memql#4937, DESIGN.md
          rule 12). Pause, Resume, Archive and Restore moved to the ACTION BAR,
          which carries the state in words and only the acts legal from it.
          They lived here at y=2412 and y=2499 on a 5,069px page, under five
          other sections, while the Head's own action sat at y=354 -- so the
          act somebody came for was the one they had to go looking for.

          What this stop keeps is the READINGS: is anybody using it, what has
          it served, and what it is configured with. Those are things to look
          at rather than things to press, and a reader who cannot write still
          sees every one of them. */}
      {/* THE REFUSAL IS THE PAGE'S NOW, NOT THIS STOP'S (epic memql#4937).
          Every act that can be refused -- publish, unpublish, archive,
          restore, delete -- moved to the bar, so the server's sentence
          belongs once, beside the rail, where whoever pressed the bar is
          looking. Rendering it here as well put the same sentence on screen
          twice. The rollback below is this stop's own act and shares the
          hook, which is exactly why the render had to move rather than the
          hook. */}
    </div>
  );
}

/** A bundle ref reads as its version segment: the content hash is what
 *  distinguishes two versions, and the prefix is the same on every one. */
function versionLabel(bundleRef: string): string {
  const trimmed = bundleRef.replace(/\/$/, "");
  const parts = trimmed.split("/");
  const last = parts[parts.length - 1] ?? "";
  return last === "" ? bundleRef : last;
}
