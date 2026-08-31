import { useEffect, useRef, useState } from "react";
import { ExternalLink, Sparkles } from "lucide-react";

import { Button, Chip, Chips, Fact, Facts, Panel, ProvenanceDot, Subhead } from "../../kit";
import { formatMoment } from "../../kit/format";
import { TICK_TTL_MS } from "../../live/arrival";
import { STOREFRONT_KIND, kindLabel } from "./concepts";
import { PublishPicker } from "./actions/PublishPicker";
import {
  bundleForm,
  bundleFormLabel,
  bundleFormNote,
  liveUrlFor,
  ownerLabel,
  siteName,
  statusDotTone,
  statusTone,
  storefrontBinding,
  type SiteRow,
} from "./rows";

// One deployable, in full.
//
// Read-only by itself. The publish half is mounted here rather than in the
// Actions section because publishing is a thing you do TO a deployable you are
// already looking at, and a picker on another screen would make somebody carry
// an id across it. `canPublish` is the manifest's admin gate restated for this
// surface -- presentation, exactly as spec section E says; the engine
// re-resolves both rows under the caller's own actor before it reads a byte.

export function SiteDetail({
  site,
  viewerUserId,
  canPublish,
  onAsk,
}: {
  site: SiteRow;
  viewerUserId: string;
  canPublish: boolean;
  /** Opens Ask with this deployable as its context. */
  onAsk?: (tag: string) => void;
}) {
  const form = bundleForm(site.bundleRef);
  const url = liveUrlFor(site.hostname);
  const storefront = site.kind === STOREFRONT_KIND ? storefrontBinding(site) : null;
  const flipped = useBundleFlip(site);

  return (
    <Panel label={`Deployable ${siteName(site)}`}>
      <div className="os-head">
        <Subhead>{siteName(site)}</Subhead>
        <div className="os-head-actions">
          {url === "" ? null : (
            /* The single most useful link on this surface: where the thing
               actually is. `rel` is not decoration -- a new tab handed a live
               `window.opener` can navigate the shell it came from. */
            <a className="os-button" data-tone="quiet" href={url} target="_blank" rel="noreferrer noopener">
              <ExternalLink size={13} aria-hidden /> Open
            </a>
          )}
          {onAsk ? (
            <Button
              onClick={() => onAsk(`app:deployables site:${site.hostname || site.id}`)}
              ariaLabel={`Ask about ${siteName(site)}`}
            >
              <Sparkles size={13} aria-hidden /> Ask
            </Button>
          ) : null}
        </div>
      </div>

      <Chips label="Deployable state">
        <span className="os-deploy-status" data-tone={statusTone(site)}>
          <ProvenanceDot tone={statusDotTone(site)} />
          {site.status || "status unknown"}
        </span>
        <Chip>{kindLabel(site.kind) || "kind unknown"}</Chip>
        <Chip tone={ownerLabel(site, viewerUserId) === "yours" ? "accent" : "muted"}>
          {ownerLabel(site, viewerUserId)}
        </Chip>
        {site.apiProxy ? (
          <Chip title="/_memql/* is mounted on this origin and forwarded to the bff, so the site is same-origin with its own API.">
            api proxy
          </Chip>
        ) : null}
        {site.systemOwned ? (
          <Chip title="Re-seeded at boot and refused at the delete path, so cluster management cannot be bricked by deleting it.">
            system-owned
          </Chip>
        ) : null}
      </Chips>

      <Facts>
        <Fact label="Hostname" value={site.hostname} mono />
        <Fact label="Kind" value={site.kind} mono />
        <Fact label="Status" value={site.status} mono />
        <Fact
          label="Bundle"
          value={
            <span className="os-deploy-bundle">
              <span className="os-deploy-bundle-form">{bundleFormLabel(form)}</span>
              <code className="os-mono">{site.bundleRef || "--"}</code>
              {/* THE FLIP, MARKED. A CI publish through POST /sites/{id}/bundles
                  happens on a node nobody in this browser is talking to; the
                  `updated` broadcast brings it here, and without a marker the
                  value would simply be different from the one somebody read a
                  moment ago with nothing to say it had moved. */}
              {flipped ? (
                <span className="os-livelist-tick" role="status">
                  changed just now
                </span>
              ) : null}
            </span>
          }
        />
        <Fact label="What that means" value={bundleFormNote(form)} />
        <Fact
          label="Published from"
          value={site.artifactId === "" ? "" : <code className="os-mono">{site.artifactId}</code>}
          title={
            site.artifactId === ""
              ? undefined
              : "Provenance only. The edge reads bundleRef and never this field."
          }
        />
        {site.artifactId === "" ? null : (
          <Fact label="Provenance" value="Published from the Library." />
        )}
        <Fact label="Title" value={site.title} />
        <Fact label="Notes" value={site.notes} />
        <Fact label="Created" value={site.createdAt === "" ? "" : formatMoment(site.createdAt)} />
      </Facts>

      {storefront === null ? null : (
        <>
          <Subhead>Shopify binding</Subhead>
          <Facts>
            <Fact label="Store" value={storefront.storeDomain} mono />
            {/* THE NAME, NEVER THE VALUE. `storefrontTokenRef` NAMES a
                v1:platform:globalSecret row; the token itself is not on this
                row and is not fetched here. The edge resolves it at serve time
                into the site's runtime-config document, and that is the only
                place it is dereferenced. */}
            <Fact
              label="Storefront token"
              value={storefront.storefrontTokenRef}
              mono
              title="The name of the secret that holds the token. The value is resolved by the edge at serve time and is never read here."
            />
          </Facts>
        </>
      )}

      {canPublish ? <PublishPicker site={site} /> : null}
    </Panel>
  );
}

/**
 * Whether this deployable's bundle changed while somebody was looking at it.
 *
 * Keyed on the VALUE rather than on the arrival cue, and the difference
 * matters: an `updated` tick fires for a rename too, so a marker driven by the
 * tick would announce "the bundle changed" on an edit that did not touch it.
 * What is being claimed here is specific, so what is watched is specific.
 *
 * It decays on the same clock as the arrival cue, because it IS the arrival
 * cue restated for one field: news arrives, announces itself once, and is gone.
 */
function useBundleFlip(site: SiteRow): boolean {
  const seen = useRef({ id: site.id, bundleRef: site.bundleRef });
  const [flipped, setFlipped] = useState(false);

  useEffect(() => {
    // A DIFFERENT DEPLOYABLE IS A BASELINE, NOT A CHANGE. Selecting another
    // site would otherwise light the marker on it, claiming a publish that
    // was only somebody clicking a second row.
    if (seen.current.id !== site.id) {
      seen.current = { id: site.id, bundleRef: site.bundleRef };
      setFlipped(false);
      return;
    }
    if (seen.current.bundleRef === site.bundleRef) return;
    seen.current = { id: site.id, bundleRef: site.bundleRef };
    setFlipped(true);
    const t = setTimeout(() => setFlipped(false), TICK_TTL_MS);
    return () => clearTimeout(t);
  }, [site.id, site.bundleRef]);

  return flipped;
}
