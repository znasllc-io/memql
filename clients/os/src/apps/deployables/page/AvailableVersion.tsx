import { useState } from "react";
import { GitBranch } from "lucide-react";
import { AttentionDestination, AttentionMarker } from "../../../attention/Attention";
import { Button } from "../../../kit";
import { updateTarget } from "../attention";
import { shortVersion, type PackageRow } from "../packages/rows";
import { usePaneActive } from "../paneActivity";

/** The same explicit acknowledgment destination, with or without a deployed site. */
export function AvailableVersion({ pkg, onUpdate }: { pkg: PackageRow; onUpdate?: () => void }) {
  const [open, setOpen] = useState(false);
  const active = usePaneActive();
  if (pkg.sourceKind !== "repo") return null;
  return <div className="deployable-version-picker">
    <button type="button" className="deployable-piece-chip os-attention-anchor" aria-expanded={open} onClick={() => setOpen(value => !value)}>
      <GitBranch size={14} aria-hidden />Available version{pkg.updateAvailable ? ` · ${shortVersion(pkg.latestKnownVersion)}` : ""}
      <AttentionMarker appId="deployables" target={updateTarget(pkg.id)} />
    </button>
    {open ? <AttentionDestination appId="deployables" sectionId="deployables" target={updateTarget(pkg.id)} visible={active}>
      <div className="deployable-version-row"><span>Upstream</span><div><strong className="os-mono">{shortVersion(pkg.latestKnownVersion) || "Not checked yet"}</strong><small>{pkg.updateAvailable ? pkg.autoDeploy ? "Automatic deployment is enabled" : "Ready when you choose to deploy" : "No newer version detected"}</small></div>{pkg.updateAvailable && onUpdate ? <Button onClick={onUpdate}>Review update</Button> : null}</div>
    </AttentionDestination> : null}
  </div>;
}
