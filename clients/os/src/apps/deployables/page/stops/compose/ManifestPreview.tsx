import { Box, FileCode2 } from "lucide-react";

import { Caption, Chip } from "../../../../../kit";
import { manifestIsEmpty, type ManifestSummary } from "../../../sources/probe";

// What the package says about itself, before anything is fetched (epic
// memql#4915, design section A step 4).
//
// ===========================================================================
// A COURTESY, IN THE REPORT'S OWN VOCABULARY
// ===========================================================================
// The probe reads memql-package.yaml through the contents API and answers a
// summary. It renders in `ReportView`'s classes -- `.os-report-part`,
// `.os-report-heading`, `.os-report-item` -- so the preview and the report
// that replaces it read as one thing rather than as two designs for the same
// facts. Nothing new was invented: what changes between them is how much is
// known, not what kind of thing is being said.
//
// ===========================================================================
// NO MANIFEST GETS NO PREVIEW AND NO COMPLAINT
// ===========================================================================
// A repository with no manifest, one that does not parse, one written for a
// format version this cluster does not read -- all answer an empty summary
// (`probeManifest`), and this renders nothing at all for every one of them.
// The ANALYSIS is the authority: it reads the real tree and reports the real
// problem in one sentence, and a warning here would report it twice, before
// the run that actually knows has said anything.
//
// It also says WHAT IT IS: "read from the manifest" and not "analyzed". The
// two answer different questions -- what the package claims, and what this
// cluster found -- and a preview that dressed as a report would be making a
// promise the run has not made yet.

export function ManifestPreview({ manifest }: { manifest: ManifestSummary }) {
  if (manifestIsEmpty(manifest)) return null;
  return (
    <section className="os-report-part">
      <h4 className="os-report-heading">
        <Box size={12} aria-hidden /> {manifest.name === "" ? "This package" : manifest.name}
      </h4>
      <Caption>
        Read from the repository&apos;s manifest. Analyze reads the tree itself and is the authority on all of it.
      </Caption>

      {manifest.deployables.length === 0 ? (
        <p className="os-caption">Its manifest declares no web apps.</p>
      ) : (
        <ul className="os-report-list">
          {manifest.deployables.map((app) => (
            <li key={app.name} className="os-report-item">
              <div className="os-report-item-head">
                <span className="os-report-name">{app.name}</span>
                {app.kind === "" ? null : <Chip>{app.kind}</Chip>}
              </div>
              {app.path === "" ? null : <p className="os-report-path">{app.path}</p>}
            </li>
          ))}
        </ul>
      )}

      {manifest.dslDomains.length === 0 ? null : (
        <div className="os-report-item">
          <div className="os-report-item-head">
            <span className="os-report-name">
              <FileCode2 size={12} aria-hidden /> MemQL this adds
            </span>
            <span className="os-caption">{manifest.dslDomains.join(", ")}</span>
          </div>
        </div>
      )}
    </section>
  );
}
