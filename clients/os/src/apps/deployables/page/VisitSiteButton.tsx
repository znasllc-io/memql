import { ExternalLink } from "lucide-react";
import { IconButton } from "../../../kit/IconButton";
import { liveUrlFor, type SiteRow } from "../rows";

/**
 * The Head's way to the live website.
 *
 * A STOREFRONT HAS TWO, Testing and Production, each against its own store,
 * so its button opens the Visit page (`VisitPage`) in the deployable's place;
 * it used to ask in a dialog. Every other kind has one address and links
 * straight to it in a new tab.
 */
export function VisitSiteButton({ site, name, onOpenVisit }: {
  site: SiteRow;
  name: string;
  /** Opens the Visit page: a storefront's choice between Testing and Production. */
  onOpenVisit: () => void;
}) {
  const url = liveUrlFor(site.hostname);
  if (!url) return null;
  if (site.kind !== "shopify_storefront") return <a className="os-icon-button" aria-label={`Open ${name} in a new tab`} title={`Open ${name} in a new tab`} href={url} target="_blank" rel="noreferrer noopener"><ExternalLink size={16} aria-hidden /></a>;
  return <IconButton label={`Visit ${name}`} onClick={onOpenVisit}><ExternalLink size={16} aria-hidden /></IconButton>;
}
