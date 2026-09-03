import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Head, Notice } from "../../../kit";
import type { ArrivalTick } from "../../../live/arrival";
import { DeployablePage } from "../page/DeployablePage";
import type { PackageRow } from "../packages/rows";
import { siteName, type SiteRow } from "../rows";
import type { CredentialRow } from "../sources/rows";
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
  viewerUserId,
  canWrite,
  isClusterOwner,
  clusterDomain,
  packages,
  credentials,
  onAsk,
  onReseed,
}: {
  sites: readonly SiteRow[];
  snapshot: LiveSnapshot<SiteRow>;
  ticks: Map<string, ArrivalTick>;
  selection: MapSelection;
  onSelectNode: (node: MapNode) => void;
  onSelectSite: (siteId: string) => void;
  viewerUserId: string;
  canWrite: boolean;
  isClusterOwner: boolean;
  /** The domain this cluster serves, threaded to the page's Domains content. */
  clusterDomain: string;
  packages: readonly PackageRow[];
  credentials: readonly CredentialRow[];
  onAsk?: (tag: string) => void;
  onReseed: () => void;
}) {
  const chosen = sites.filter((s) => selection.siteIds.includes(s.id));
  const only = chosen.length === 1 ? (chosen[0] ?? null) : null;
  const pkg = only === null || only.packageId === "" ? null : (packages.find((p) => p.id === only.packageId) ?? null);
  const siblings =
    only === null || only.packageId === "" ? [] : sites.filter((s) => s.packageId === only.packageId && s.id !== only.id);

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
        <DeployablePage
          key={only.id}
          site={only}
          pkg={pkg}
          siblings={siblings}
          credentials={credentials}
          viewerUserId={viewerUserId}
          canWrite={canWrite}
          isClusterOwner={isClusterOwner}
          clusterDomain={clusterDomain}
          onAsk={onAsk}
        />
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
