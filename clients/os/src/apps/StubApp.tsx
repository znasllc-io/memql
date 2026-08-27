import type { OsAppManifest } from "../system/registry";

// The honest stub (spec D12): a real window whose body says exactly what
// this app is, which epic builds it, and which sections it declares. The
// shell never fakes functionality -- when an app epic lands, its component
// replaces this body and nothing else changes.

export function StubApp({
  manifest,
  epicIssue,
  summary,
}: {
  manifest: OsAppManifest;
  epicIssue: number;
  summary: string;
}) {
  const Icon = manifest.icon;
  return (
    <div className="os-stub">
      <div className="os-stub-mark">
        <Icon size={30} aria-hidden />
      </div>
      <h3 className="os-stub-title">{manifest.name}</h3>
      <p className="os-stub-summary">{summary}</p>
      <p className="os-caption">
        Arrives with epic #{epicIssue}. This window, its sections and its dock
        presence are the real shell contracts.
      </p>
    </div>
  );
}
