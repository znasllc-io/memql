import { Caption, Fact, Facts } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import type { SiteVersion } from "../packages/calls";
import type { SiteRow } from "../rows";
import type { VersionState } from "./Versions";

/**
 * One version of a deployable, as a page (design record
 * 2026-11-08-site-and-email-statistics, section 10).
 *
 * WHAT THE VERSION IS, AND NOTHING THAT CHANGES IT. The Head, the trail
 * (`Deployables > <name> > Version <label>`) and the bar are the deployable
 * page's, and so is the one act: Roll back to this version stands on the bar
 * when `rollBackLegal` says it may and is absent when it may not (DESIGN.md
 * rule 12), and a refused roll back stands under these facts, where the
 * deployable page draws every refusal. Nothing on the page itself is
 * pressable.
 *
 * `state` and `served` are what the version is NOW: the caller reads them
 * from the live row on every render, so a roll back that lands while the
 * page is open turns it to Current rather than leaving the reading taken
 * when it opened.
 */
export function VersionPage({ version, state, served, bundled }: {
  /** The deployable the version belongs to. */
  site: SiteRow;
  version: SiteVersion;
  state: VersionState;
  /** Whether the deployable has ever served it: a candidate that never did is no version to go back to. */
  served: boolean;
  /** The deployable serves a build MemQL ships, which keeps no versions of its own. */
  bundled: boolean;
}) {
  // SAID ONCE (DESIGN.md rule 7): the Head names the version, so its label is
  // not a fact here as well.
  return <>
    <Facts>
      <Fact label="State" value={state} />
      <Fact label="Bundle" value={version.bundleRef} />
      {version.createdAt ? <Fact label="Recorded" value={formatMoment(version.createdAt)} /> : null}
    </Facts>
    {bundled ? <Caption>This app is included with the installed MemQL build. Earlier builds are managed with the cluster release.</Caption> : null}
    {!served ? <Caption>Deployed as the candidate and not served yet. Promote the candidate to serve it.</Caption> : null}
  </>;
}
