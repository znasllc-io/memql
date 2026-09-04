import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Head, Notice } from "../../../kit";
import type { ArrivalTick } from "../../../live/arrival";
import { sourceLabel, type PackageRow } from "../packages/rows";
import { liveUrlFor, siteName, statusTone, type SiteRow } from "../rows";
import { DeployMap } from "./DeployMap";
import type { MapNode } from "./layout";

// The Map section: the picture, and whatever it is pointing at.
//
// The map and the Deployables list are two readings of ONE feed and share ONE
// selection, so walking a cluster on the map and then switching to the list
// lands on the same deployable. A second selection would let the two disagree
// about what is being looked at -- in an app whose whole subject is a thing
// being in more than one place at once.

export interface MapSelection {
  /** The node, for highlighting. "" when nothing is selected. */
  nodeId: string;
  /**
   * The deployables that node is part of, captured when it was chosen.
   *
   * IDS RATHER THAN THE NODE: the layout re-derives on every event, so a held
   * node object would be a snapshot of a shape that has since moved. An id
   * survives, and the row it names is looked up in the CURRENT rows -- so a
   * deployable that has since gone simply resolves to nothing rather than
   * rendering a page for a row that is no longer there.
   */
  siteIds: string[];
}

export const NO_SELECTION: MapSelection = { nodeId: "", siteIds: [] };

export function MapSection({
  sites,
  snapshot,
  ticks,
  selection,
  onSelectNode,
  onSelectSite,
  onOpenDeployable,
  packages,
  onReseed,
}: {
  sites: readonly SiteRow[];
  snapshot: LiveSnapshot<SiteRow>;
  ticks: Map<string, ArrivalTick>;
  selection: MapSelection;
  onSelectNode: (node: MapNode) => void;
  onSelectSite: (siteId: string) => void;
  /**
   * Opens a deployable's own page, in the Deployables section.
   *
   * The map no longer renders one itself (rule 11): it points, and the page
   * lives where the page lives. Everything the old inline page needed --
   * credentials, the cluster domain, the write gate -- went with it.
   */
  onOpenDeployable: (siteId: string) => void;
  packages: readonly PackageRow[];
  onReseed: () => void;
}) {
  const chosen = sites.filter((s) => selection.siteIds.includes(s.id));
  const only = chosen.length === 1 ? (chosen[0] ?? null) : null;
  const pkg = only === null || only.packageId === "" ? null : (packages.find((p) => p.id === only.packageId) ?? null);

  return (
    <div className="os-app-stack">
      <Head title="Deploy map" />

      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its deployables, so there is nothing to draw."
          next="An empty canvas would be a claim rather than a drawing, so the map says this instead."
        >
          <Button onClick={onReseed}>Try again</Button>
        </Notice>
      ) : null}

      <DeployMap
        sites={sites}
        ticks={ticks}
        state={snapshot.state}
        selectedNodeId={selection.nodeId}
        onSelect={onSelectNode}
      />

      {only ? (
        /* THE MAP POINTS; THE PAGE IS ELSEWHERE (DESIGN.md rule 11, epic
           memql#4937). This used to render the WHOLE DeployablePage beneath
           the picture -- 5,000px of rail, settings, domains and history under
           a drawing whose whole job is to answer "which host, which site,
           which bundle" at a glance. The same fault the list had, in the same
           app.

           A card answers what the map raised, and Open takes you to the one
           page that owns the deployable. */
        <div className="os-panel os-map-card">
          <div className="os-map-card-head">
            <span className="os-map-card-name">{siteName(only)}</span>
            <span className="os-mono os-map-card-host">{only.hostname}</span>
            <span className="os-map-card-state" data-tone={statusTone(only)}>
              {mapStatusWord(only.status)}
            </span>
          </div>
          {pkg === null ? null : <Caption>From {sourceLabel(pkg)}</Caption>}
          <div className="os-form-row">
            <Button tone="primary" onClick={() => onOpenDeployable(only.id)}>
              Open this deployable
            </Button>
            {liveUrlFor(only.hostname) === "" ? null : (
              <a
                className="os-button"
                data-tone="quiet"
                href={liveUrlFor(only.hostname)}
                target="_blank"
                rel="noreferrer noopener"
              >
                Visit
              </a>
            )}
          </div>
        </div>
      ) : chosen.length > 1 ? (
        /* A bundle serving several deployables. There is no ONE page to
           open, so the choice is offered rather than made arbitrarily -- and
           the fact itself, that one bundle is serving all of these, is the
           thing the map was drawn to show. */
        <div className="os-panel">
          <Caption>That node serves more than one deployable. Pick the one to open.</Caption>
          <div className="os-form-row">
            {chosen.map((site) => (
              <Button key={site.id} onClick={() => onSelectSite(site.id)}>
                {siteName(site)}
              </Button>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}

/** The map's own status word, matching the bar's vocabulary (D6). */
function mapStatusWord(status: string): string {
  switch (status) {
    case "live":
      return "published";
    case "disabled":
      return "unpublished";
    case "archived":
      return "archived";
    default:
      return "draft";
  }
}
