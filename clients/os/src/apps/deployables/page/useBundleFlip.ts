import { useEffect, useRef, useState } from "react";

import { TICK_TTL_MS } from "../../../live/arrival";
import type { SiteRow } from "../rows";

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
 *
 * It lived on the detail panel before the page existed (memql#4725) and moved
 * here with the Source stop, which is where the bundle reference renders now:
 * a CI push through POST /sites/{id}/bundles is the source ARRIVING, and the
 * marker says so beside the reference it changed.
 */
export function useBundleFlip(site: SiteRow): boolean {
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
