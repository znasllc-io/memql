import { useRef, useState } from "react";
import { FlaskConical, Globe } from "lucide-react";
import { Button, Caption, Notice, RecordList, RecordRow } from "../../../kit";
import type { PartsHeld } from "../parts";
import { liveUrlFor, type SiteRow } from "../rows";
import { type PreviewReadiness } from "../preview/rows";
import { usePreviewWrites } from "../preview/usePreview";

/**
 * Where a storefront can be visited: Testing and Production, each against its
 * own store (design record 2026-11-08-site-and-email-statistics, section 10).
 *
 * A PAGE, WHERE IT USED TO BE A DIALOG. It takes the deployable page's place
 * under `Deployables > <name> > Visit`, so the trail says where the person is
 * and Back steps out. Opening either website leaves the page standing: the
 * dialog closed itself because a dialog is in the way, and a page is not.
 * Every other kind of deployable has one address, and its Head links straight
 * to it (`VisitSiteButton`).
 */
export function VisitPage({ site, can, readiness, readinessError, onWritten, onOpenStore }: {
  site: SiteRow;
  can: PartsHeld;
  readiness: PreviewReadiness | null;
  readinessError: string;
  onWritten: () => void;
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
  return <>
    <RecordList as="ul" label="Website destinations">
      <RecordRow icon={<FlaskConical size={18} aria-hidden />} name="Testing" secondary={readiness?.previewStoreDomain || "No store connected"} state={testingState} tone={canTest ? "accent" : "muted"} label="Open testing website" onOpen={busy ? undefined : canTest ? () => void visitTesting() : undefined} />
      <RecordRow icon={<Globe size={18} aria-hidden />} name="Production" secondary={!readiness ? "Reading store connection…" : readiness.storeDomain || (readiness.storeId ? "Store unavailable" : "No production store connected")} state={productionState} label="Open production website" onOpen={!busy && site.status === "live" ? () => {
        window.open(liveUrlFor(site.hostname), "_blank", "noopener,noreferrer");
      } : undefined} />
    </RecordList>
    {readiness?.testingUrl ? <div className="os-app-stack"><Caption>Testing: {readiness.testingUrl}</Caption><Caption>Production: {liveUrlFor(site.hostname)}</Caption></div> : null}
    {readinessError ? <Notice tone="error" sentence="Store connections could not be read."><Button onClick={onWritten}>Try again</Button></Notice> : null}
    {readiness?.previewRefusal.code ? <Notice sentence="Testing needs attention." detail={readiness.previewRefusal.message}>{can.store ? <Button onClick={onOpenStore}>Open store settings</Button> : null}</Notice> : null}
    {writes.error ? <Notice tone="error" sentence="The testing website could not be opened." detail={writes.error} /> : null}
    {fallbackUrl ? <><Caption>Open the testing website in a new tab.</Caption><a href={fallbackUrl} target="_blank" rel="noreferrer noopener">Open testing website</a></> : null}
  </>;
}
