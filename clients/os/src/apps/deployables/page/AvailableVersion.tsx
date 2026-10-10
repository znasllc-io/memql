import { useState } from "react";
import { GitBranch } from "lucide-react";
import { Button } from "../../../kit";
import { shortVersion, type PackageRow } from "../packages/rows";

/** Inspect the available version, with or without a deployed site. */
export function AvailableVersion({ pkg, onUpdate }: { pkg: PackageRow; onUpdate?: () => void }) {
  const [open, setOpen] = useState(false);
  if (pkg.sourceKind !== "repo") return null;
  return <div className="deployable-version-picker">
    <button type="button" className="deployable-piece-chip" aria-expanded={open} onClick={() => setOpen(value => !value)}>
      <GitBranch size={14} aria-hidden />Available version{pkg.updateAvailable ? ` · ${shortVersion(pkg.latestKnownVersion)}` : ""}

    </button>
    {open ?
      <div className="deployable-version-row"><span>Upstream</span><div><strong className="os-mono">{shortVersion(pkg.latestKnownVersion) || "Not checked yet"}</strong><small>{pkg.updateAvailable ? pkg.autoDeploy ? "Automatic deployment is enabled" : "Ready when you choose to deploy" : "No newer version detected"}</small></div>{pkg.updateAvailable && onUpdate ? <Button onClick={onUpdate}>Review update</Button> : null}</div>
     : null}
  </div>;
}
