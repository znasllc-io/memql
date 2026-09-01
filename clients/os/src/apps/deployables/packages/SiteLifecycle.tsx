import { useCallback, useEffect, useState } from "react";
import { Archive, History, Pause, Play, RotateCcw, Undo2 } from "lucide-react";

import { Button, Caption, Chip, Input } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { useOsConnection } from "../../../live/connection";
import { useSiteLifecycle } from "./actions";
import { fetchSiteVersions, MAX_HISTORY_VERSIONS, type SiteVersion } from "./calls";
import { ProblemNotice } from "./ReportView";
import type { SiteRow } from "../rows";

// A deployable's lifecycle, migrated from the portal (design D13).
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
// THE VERSION LIST IS THE ROW'S OWN HISTORY, WALKED
// ===========================================================================
// There is no all-versions query, because "the graph's own history is the
// version list" -- so this re-issues siteById under successive `asOf`
// timestamps, and each entry it finds is a bundleRef that can be pointed back
// at. Loaded on DEMAND rather than with the panel: each version is a round
// trip, and most people opening a deployable are not rolling it back.
//
// ===========================================================================
// RAW bundleRef POINTING STAYS OUT
// ===========================================================================
// Deliberately (D13). Parity covers the four features somebody uses -- history,
// rollback, enable/disable, archive -- and not the operator escape hatch. A
// text field that accepts any URI is a way to point a live site at nothing.

const SYSTEM_OWNED_NOTE =
  "This is one of the cluster's own surfaces. It is re-seeded live at every boot, so it has no lifecycle to change.";

export function SiteLifecycle({ site, canWrite }: { site: SiteRow; canWrite: boolean }) {
  const connection = useOsConnection();
  const actions = useSiteLifecycle();
  const [versions, setVersions] = useState<SiteVersion[] | null>(null);
  const [loadingVersions, setLoadingVersions] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const [confirmHostname, setConfirmHostname] = useState("");

  const loadVersions = useCallback(async () => {
    if (connection === null) return;
    setLoadingVersions(true);
    try {
      setVersions(await fetchSiteVersions(connection.query, site.id));
    } catch {
      // A history that could not be read is an absent history, not a broken
      // panel: the rest of this surface still works and the walk can be
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
    setArchiving(false);
    setConfirmHostname("");
  }, [site.id]);

  if (site.systemOwned) {
    return (
      <section className="os-report-part">
        <h4 className="os-report-heading">Lifecycle</h4>
        <Caption>{SYSTEM_OWNED_NOTE}</Caption>
      </section>
    );
  }

  if (!canWrite) return null;

  const archived = site.status === "archived";
  const live = site.status === "live";

  return (
    <>
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
                    onClick={() => void actions.rollTo(site.id, v.bundleRef)}
                    busy={actions.busy}
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

      <section className="os-report-part">
        <h4 className="os-report-heading">Availability</h4>
        {archived ? (
          <>
            <Caption>
              Archived deployables answer nothing. Restoring brings this one back paused, so publishing it again is its
              own decision.
            </Caption>
            <Button onClick={() => void actions.restore(site.id)} busy={actions.busy}>
              <RotateCcw size={12} aria-hidden /> Restore, paused
            </Button>
          </>
        ) : (
          <>
            <Caption>
              {live
                ? "Live. Pausing answers 503 rather than 404, so a deliberately paused site stays distinguishable from a typo."
                : "Not serving. Anyone visiting gets nothing until this is live."}
            </Caption>
            <Button
              onClick={() => void actions.setStatus(site.id, live ? "disabled" : "live")}
              busy={actions.busy}
              ariaLabel={live ? `Pause ${site.hostname}` : `Make ${site.hostname} live`}
            >
              {live ? (
                <>
                  <Pause size={12} aria-hidden /> Pause
                </>
              ) : (
                <>
                  <Play size={12} aria-hidden /> Make it live
                </>
              )}
            </Button>
          </>
        )}
      </section>

      {archived ? null : (
        <section className="os-report-part os-danger-part">
          <h4 className="os-report-heading">
            <Archive size={12} aria-hidden /> Archive
          </h4>
          {archiving ? (
            <>
              <Caption>
                Archiving keeps the deployable and its whole history. It has to be paused first. Type{" "}
                <strong>{site.hostname}</strong> to confirm.
              </Caption>
              <div className="os-confirm-row">
                <Input
                  id={`os-archive-site-${site.id}`}
                  label={`Type ${site.hostname} to confirm`}
                  value={confirmHostname}
                  onChange={setConfirmHostname}
                  placeholder={site.hostname}
                />
                <Button tone="quiet" onClick={() => setArchiving(false)}>
                  Cancel
                </Button>
                <Button
                  tone="danger"
                  disabled={confirmHostname !== site.hostname}
                  busy={actions.busy}
                  onClick={() => void actions.archive(site.id, confirmHostname)}
                >
                  Archive
                </Button>
              </div>
            </>
          ) : (
            <>
              <Caption>
                {live
                  ? "Pause it first. Archiving is the end of a deployable's life, and pausing is what gives anyone still using it a chance to notice."
                  : "An archived deployable stays listed under the Archived filter and can be restored. Nothing is deleted."}
              </Caption>
              <Button onClick={() => setArchiving(true)} disabled={live}>
                <Archive size={12} aria-hidden /> Archive this deployable
              </Button>
            </>
          )}
        </section>
      )}

      {/* The server is the law, and it says so here: a refusal renders in
          place, in the server's own words. The flow never suppresses one --
          the disable-first rule and the systemOwned exemption are checked
          again beside the engine's write path, so a refusal that arrives
          despite this UI having allowed the click is the interesting case. */}
      {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}
    </>
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
