import { usePublishAttention } from "../../attention/Attention";
import type { AttentionChange } from "../../attention/model";
import { packageFromRow, type PackageRow } from "./packages/rows";
import { usePackages } from "./packages/usePackages";

export const updateTarget = (packageId: string) => `package:${packageId}:version`;
export function packageAttention(pkg: PackageRow): AttentionChange[] {
  if (pkg.sourceKind !== "repo" || pkg.status !== "active" || pkg.autoDeploy || !pkg.updateAvailable || !pkg.latestKnownVersion || pkg.latestKnownVersion === pkg.deployedVersion) return [];
  return [{ id: updateTarget(pkg.id), revision: pkg.latestKnownVersion, appId: "deployables", sectionId: "deployables", target: updateTarget(pkg.id), ancestors: ["map"], label: `New version for ${pkg.name || pkg.repoUrl}`, kind: "runtime" }];
}
/** Lives with the shell, so closed apps still report real discoveries. */
export function DeployablesAttentionFeed() {
  const { snapshot } = usePackages();
  usePublishAttention("deployables:updates", snapshot.rows.flatMap(row => packageAttention(packageFromRow(row))));
  return null;
}
