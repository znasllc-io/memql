import { useRef, useState } from "react";
import { ExternalLink, FlaskConical, Globe } from "lucide-react";
import { Button, Caption, Notice, RecordList, RecordRow } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import type { PartsHeld } from "../parts";
import { liveUrlFor, type SiteRow } from "../rows";
import { type PreviewReadiness } from "../preview/rows";
import { usePreviewWrites } from "../preview/usePreview";
import { DetailDialog } from "./DetailDialog";

export function VisitSiteButton({ site, name, can, readiness, readinessError, onWritten, onOpenStore }: {
  site: SiteRow;
  name: string;
  can: PartsHeld;
  readiness: PreviewReadiness | null;
  readinessError: string;
  onWritten: () => void;
  onOpenStore: () => void;
}) {
  const [choosing, setChoosing] = useState(false);
  const url = liveUrlFor(site.hostname);
  if (!url) return null;
  if (site.kind !== "shopify_storefront") return <a className="os-icon-button" aria-label={`Open ${name} in a new tab`} title={`Open ${name} in a new tab`} href={url} target="_blank" rel="noreferrer noopener"><ExternalLink size={16} aria-hidden /></a>;
  return <>
    <IconButton label={`Visit ${name}`} aria-haspopup="dialog" onClick={() => setChoosing(true)}><ExternalLink size={16} aria-hidden /></IconButton>
    {choosing ? <StorefrontDestinations key={site.id} site={site} can={can} readiness={readiness} readinessError={readinessError} onWritten={onWritten} onClose={() => setChoosing(false)} onOpenStore={() => { setChoosing(false); onOpenStore(); }} /> : null}
  </>;
}

function StorefrontDestinations({ site, can, readiness, readinessError, onWritten, onClose, onOpenStore }: {
  site: SiteRow;
  can: PartsHeld;
  readiness: PreviewReadiness | null;
  readinessError: string;
  onWritten: () => void;
  onClose: () => void;
  onOpenStore: () => void;
}) {
  const writes = usePreviewWrites(onWritten);
  const opening = useRef(false);
  const [busy, setBusy] = useState(false);
  const [fallbackUrl, setFallbackUrl] = useState("");
  const version = readiness?.bundleRef || "";

  const canTest = can.preview && !site.systemOwned && Boolean(version) && Boolean(readiness?.testingUrl) && readiness?.canPreview === true;

  async function visitTesting() {
    if (!canTest || !readiness || opening.current) return;
    opening.current = true;
    setBusy(true);
    setFallbackUrl("");
    // Reserve the tab during the click, before the cluster round trips. Detach
    // the opener while it is still blank, before navigating to the storefront.
    const tab = window.open("about:blank", "_blank");
    if (tab) tab.opener = null;
    try {
      const preview = await writes.openPreview(site.id);
      if (!preview?.url) {
        tab?.close();
        return;
      }
      if (tab && !tab.closed) {
        tab.location.replace(preview.url);
        onClose();
      } else {
        // A blocked or closed tab must not silently turn Testing into a visit
        // to the public site. Keep the minted link in memory for a direct click.
        setFallbackUrl(preview.url);
      }
    } finally {
      opening.current = false;
      setBusy(false);
    }
  }

  const testingState = busy ? "Opening…" : !can.preview ? "Access required" : !readiness ? "Reading" : !version ? "No version yet" : readiness.previewRefusal.code ? "Needs attention" : readiness.previewStoreId ? "Connected" : "Design preview";
  const productionState = site.status !== "live" ? "Not published" : !readiness ? "Published" : readiness.storeId ? "Published" : "Design preview";
  return <DetailDialog title="Visit website" onClose={onClose}>
    <RecordList as="ul" label="Website destinations">
      <RecordRow icon={<FlaskConical size={18} aria-hidden />} name="Testing" secondary={readiness?.previewStoreDomain || "No store connected"} state={testingState} tone={canTest ? "accent" : "muted"} label="Open testing website" onOpen={busy ? undefined : canTest ? () => void visitTesting() : undefined} />
      <RecordRow icon={<Globe size={18} aria-hidden />} name="Production" secondary={!readiness ? "Reading store connection…" : readiness.storeDomain || (readiness.storeId ? "Store unavailable" : "No production store connected")} state={productionState} label="Open production website" onOpen={!busy && site.status === "live" ? () => {
        window.open(liveUrlFor(site.hostname), "_blank", "noopener,noreferrer");
        onClose();
      } : undefined} />
    </RecordList>
    {readiness?.testingUrl ? <div className="os-app-stack"><Caption>Testing: {readiness.testingUrl}</Caption><Caption>Production: {liveUrlFor(site.hostname)}</Caption></div> : null}
    {readinessError ? <Notice tone="error" sentence="Store connections could not be read."><Button onClick={onWritten}>Try again</Button></Notice> : null}
    {readiness?.previewRefusal.code ? <Notice sentence="Testing needs attention." detail={readiness.previewRefusal.message}>{can.store ? <Button onClick={onOpenStore}>Open store settings</Button> : null}</Notice> : null}
    {writes.error ? <Notice tone="error" sentence="The testing website could not be opened." detail={writes.error} /> : null}
    {fallbackUrl ? <><Caption>Open the testing website in a new tab.</Caption><a href={fallbackUrl} target="_blank" rel="noreferrer noopener">Open testing website</a></> : null}
  </DetailDialog>;
}
