import { generateNickname } from "./nickname";
import { normalizeHostname } from "../domains";
import type { AddressDraft } from "../page/compose";
import type { DeployableOutcome, PackageManifest, ReportDeployable } from "./rows";

export function domainNames(value: string): string[] {
  return [...new Set(value.split(/[\s,]+/).filter(Boolean).map(v => normalizeHostname(v) || v.trim()))];
}

export function addressFromManifest(app: ReportDeployable | undefined, accountId: string): AddressDraft {
  return { slug: app?.deployment?.slug || generateNickname(), accountId, ownDomain: app?.deployment?.domains?.join(", ") || "" };
}

/** Keep the complete analyzed manifest, including build/asset declarations.
 * Only explicit address choices are overlaid. Export never writes a repository. */
export function configuredManifest(manifest: PackageManifest, addresses: Readonly<Record<string, AddressDraft>>, outcomes: readonly DeployableOutcome[], clusterDomain: string): PackageManifest {
  return { ...manifest, deployables: manifest.deployables.map(app => {
    const address = addresses[app.name];
    const outcome = outcomes.find(o => o.name === app.name && o.siteId);
    const suffix = `.${clusterDomain}`;
    const slug = outcome?.hostname.endsWith(suffix) ? outcome.hostname.slice(0, -suffix.length) : address?.slug.trim();
    if (address?.skip || (!slug && !address)) return app;
    return { ...app, deployment: { ...app.deployment, ...(slug ? {slug} : {}), ...(address ? {domains: domainNames(address.ownDomain)} : {}) } };
  }) };
}

/** JSON string scalars are valid YAML scalars: punctuation and newlines stay data. */
export function manifestYaml(value: unknown, indent = 0): string {
  const pad = " ".repeat(indent);
  if (Array.isArray(value)) return value.length ? value.map(v => typeof v === "object" && v !== null ? `${pad}-\n${manifestYaml(v, indent + 2)}` : `${pad}- ${JSON.stringify(v)}\n`).join("") : `${pad}[]\n`;
  if (value !== null && typeof value === "object") return Object.entries(value).filter(([,v]) => v !== undefined).map(([key,v]) => {
    const empty = v !== null && typeof v === "object" && Object.keys(v).length === 0;
    return `${pad}${key}:` + (v !== null && typeof v === "object" && !empty ? `\n${manifestYaml(v, indent + 2)}` : ` ${JSON.stringify(v)}\n`);
  }).join("");
  return `${pad}${JSON.stringify(value)}\n`;
}

export function downloadManifest(manifest: PackageManifest, addresses: Readonly<Record<string, AddressDraft>>, outcomes: readonly DeployableOutcome[], clusterDomain: string): void {
  const blob = new Blob([manifestYaml(configuredManifest(manifest, addresses, outcomes, clusterDomain))], {type: "application/yaml"});
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = "memql-package.yaml";
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
