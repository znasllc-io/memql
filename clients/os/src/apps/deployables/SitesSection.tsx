import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { Globe } from "lucide-react";

import { Button, Chip, Head, LiveList, Notice, ProvenanceDot, Row as ListRow } from "../../kit";
import type { LiveView } from "../../live/liveView";
import { SiteDetail } from "./SiteDetail";
import { kindLabel } from "./concepts";
import {
  bundleForm,
  bundleFormLabel,
  ownerLabel,
  siteFingerprint,
  siteIsCurrent,
  statusDotTone,
  siteName,
  statusTone,
  type SiteRow,
} from "./rows";
import type { ListDensity } from "./settings";

// The deployables of this cluster, live.
//
// The exemplar this app exists for is here: somebody's CI publishes a bundle
// through `POST /sites/{id}/bundles` on a node nobody in this browser is
// talking to, `graph.node.updated.v1:platform:site` broadcasts, and the row
// pulses under whoever is watching. Nothing polls and nothing refetches.

export function SitesSection({
  source,
  snapshot,
  density,
  selectedSiteId,
  onSelectSite,
  viewerUserId,
  canPublish,
  clusterDomain,
  canManage = false,
  onAsk,
  onReseed,
}: {
  source: LiveView<SiteRow> | null;
  snapshot: LiveSnapshot<SiteRow>;
  density: ListDensity;
  selectedSiteId: string;
  onSelectSite: (siteId: string) => void;
  viewerUserId: string;
  canPublish: boolean;
  /** The domain this cluster serves, threaded to the detail's Domains panel. */
  clusterDomain: string;
  /** Whether to render the lifecycle controls -- presentation over a
   *  server-side law, so hiding them is UX and the guard is the gate. */
  canManage?: boolean;
  onAsk?: (tag: string) => void;
  onReseed: () => void;
}) {
  const open = source?.snapshot.rows.find((s) => s.id === selectedSiteId) ?? null;

  return (
    <div className="os-app-stack" data-density={density}>
      <Head title="Sites" />

      {/* A read this surface is not allowed to make comes back as a refusal on
          the feed, not as an empty list. NO `detail`: LiveList already prints
          `snapshot.error` verbatim directly beneath the list it belongs to, and
          repeating it a few lines up reads as two different failures. */}
      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its deployables."
          next="The engine decides which sites reach you -- your own, or every one of them if you are a cluster owner."
        >
          <Button onClick={onReseed}>Try again</Button>
        </Notice>
      ) : null}

      <LiveList<SiteRow>
        source={source}
        rowId={(s) => s.id}
        // THE SAME definition the map draws from (`siteFingerprint`), not a
        // second copy of it -- see its header.
        fingerprint={siteFingerprint}
        label="Deployables in this cluster"
        emptyText="No deployables yet. Create one from the Actions section, or publish a bundle to one from the Library."
        renderRow={(site, tick) => (
          <SiteLine
            site={site}
            tick={tick}
            viewerUserId={viewerUserId}
            open={selectedSiteId === site.id}
            onToggle={() => onSelectSite(selectedSiteId === site.id ? "" : site.id)}
          />
        )}
      />

      {open === null ? null : (
        <SiteDetail
          key={open.id}
          site={open}
          viewerUserId={viewerUserId}
          canPublish={canPublish}
          clusterDomain={clusterDomain}
          canManage={canManage}
          onAsk={onAsk}
        />
      )}
    </div>
  );
}

function SiteLine({
  site,
  tick,
  viewerUserId,
  open,
  onToggle,
}: {
  site: SiteRow;
  tick: "added" | "updated" | null;
  viewerUserId: string;
  open: boolean;
  onToggle: () => void;
}) {
  const live = siteIsCurrent(site);
  const form = bundleForm(site.bundleRef);
  return (
    <ListRow
      icon={<Globe size={16} aria-hidden />}
      name={siteName(site)}
      current={live}
      dim={site.status === "disabled"}
      open={open}
      onOpen={onToggle}
      state={
        <>
          {/* The dot and the word say one thing, so only one of them says it
              to a screen reader: an unlabelled dot is aria-hidden, and the
              word is right there. Two labels would announce the status twice. */}
          <span className="os-deploy-status" data-tone={statusTone(site)}>
            <ProvenanceDot tone={statusDotTone(site)} />
            {site.status || "unknown"}
          </span>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      <Chip tone="muted">{kindLabel(site.kind) || "kind unknown"}</Chip>
      <Chip title={site.bundleRef}>{bundleFormLabel(form)}</Chip>
      {/* WHOSE IT IS. An empty ownerUserId is the meaningful CLUSTER-OWNED
          state -- the seeded portal row is the one the platform ships that way
          -- rather than a row that failed to record an owner. */}
      <Chip tone={ownerLabel(site, viewerUserId) === "yours" ? "accent" : "muted"}>
        {ownerLabel(site, viewerUserId)}
      </Chip>
    </ListRow>
  );
}
