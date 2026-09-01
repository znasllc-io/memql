import { useMemo, useState } from "react";
import { Rocket } from "lucide-react";

import { Button, Caption, Input, Notice, Panel, Subhead } from "../../../kit";
import { validateSlug } from "../hostname";
import { hostnameFor, suggestSlug } from "./hostname";
import { ReportView } from "./ReportView";
import { StageRail } from "./StageRail";
import type { DeploymentRow, PackageRow } from "./rows";

// The confirm gate (design D12): what deploying would do, and the one control
// that does it.
//
// ===========================================================================
// IN SURFACE, NEVER A DIALOG
// ===========================================================================
// A modal would be the reflex here, and it would be wrong twice over. The
// report is the size of a page, not a sentence -- a scrolling dialog is a
// worse page. And the OS's own rule is that a refusal renders beside the
// control that produced it; a refusal inside a modal that then closes is a
// refusal nobody can re-read.
//
// ===========================================================================
// THE HOSTNAME PICKER APPEARS ONCE, AND ONLY ONCE
// ===========================================================================
// A hostname is chosen at a deployable's FIRST deploy and remembered on its
// site row. So this asks only for the deployables that have never been
// deployed, and a redeploy is genuinely one click -- which is the difference
// between a gate that protects somebody and a gate they learn to click past.

export interface ConfirmGateProps {
  pkg: PackageRow;
  deployment: DeploymentRow;
  /** Deployable names that already have a site, so no hostname is asked for. */
  deployedNames: readonly string[];
  domain: string;
  busy: boolean;
  onConfirm: (hostnames: Record<string, string>) => void;
  onCancel: () => void;
}

export function ConfirmGate({
  pkg,
  deployment,
  deployedNames,
  domain,
  busy,
  onConfirm,
  onCancel,
}: ConfirmGateProps) {
  const report = deployment.report;
  const needHostname = useMemo(() => {
    const declared = report?.deployables ?? [];
    return declared.filter((d) => d.problem === undefined && !deployedNames.includes(d.name));
  }, [report, deployedNames]);

  const [slugs, setSlugs] = useState<Record<string, string>>(() => {
    const seed: Record<string, string> = {};
    for (const d of needHostname) seed[d.name] = suggestSlug(pkg.name, d.name);
    return seed;
  });

  const blocked = (report?.problems ?? []).some((p) => p.fatal);
  // "" from validateSlug means valid. An EMPTY field is not valid here either
  // -- it just has nothing to complain about yet -- so readiness checks the
  // value as well as the verdict.
  const ready =
    !blocked &&
    needHostname.every((d) => {
      const slug = (slugs[d.name] ?? "").trim();
      return slug !== "" && validateSlug(slug, domain) === "";
    });

  return (
    <Panel label={`Deploy ${pkg.name}`}>
      <div className="os-head">
        <Subhead>Deploy {pkg.name}</Subhead>
        <div className="os-head-actions">
          <Button tone="quiet" onClick={onCancel} disabled={busy}>
            Not now
          </Button>
          <Button
            tone="primary"
            onClick={() => onConfirm(hostnamesFrom(slugs, needHostname.map((d) => d.name), domain))}
            disabled={!ready}
            busy={busy}
            busyLabel="Deploying"
            ariaLabel={`Deploy ${pkg.name}`}
          >
            <Rocket size={13} aria-hidden /> Deploy
          </Button>
        </div>
      </div>

      <Caption>
        {blocked
          ? "This package cannot deploy yet. Everything below is what the cluster found."
          : "This is what deploying will do. Nothing has run yet."}
      </Caption>

      {/* The rail, here, is a FORECAST rather than a record: it shows which
          stages this run will pass through, which is how somebody learns
          before the fact that an app-only package restarts nothing. */}
      <StageRail deployment={deployment} />

      <ReportView report={report} />

      {needHostname.length > 0 ? (
        <section className="os-report-part">
          <h4 className="os-report-heading">Where each app will live</h4>
          <Caption>Chosen once. A later deploy of this package keeps the same addresses.</Caption>
          {needHostname.map((d) => {
            const slug = slugs[d.name] ?? "";
            const problem = slug.trim() === "" ? "" : validateSlug(slug, domain);
            const ok = slug.trim() !== "" && problem === "";
            const host = hostnameFor(slug, domain);
            return (
              <div key={d.name} className="os-host-pick">
                <label className="os-host-name" htmlFor={`os-host-${d.name}`}>
                  {d.name}
                </label>
                <Input
                  id={`os-host-${d.name}`}
                  label={`Address for ${d.name}`}
                  value={slug}
                  onChange={(next) => setSlugs((prev) => ({ ...prev, [d.name]: next }))}
                  placeholder="my-app"
                />
                <p className="os-host-preview" data-ok={ok ? "true" : "false"}>
                  {ok ? host : problem || "Its address will be shown here."}
                </p>
              </div>
            );
          })}
        </section>
      ) : (
        <Notice
          tone="info"
          sentence="Every app in this package already has an address."
          next="Deploying replaces what each one is serving. The addresses do not change."
        />
      )}
    </Panel>
  );
}

function hostnamesFrom(slugs: Record<string, string>, names: readonly string[], domain: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const name of names) {
    const host = hostnameFor(slugs[name] ?? "", domain);
    if (host !== "") out[name] = host;
  }
  return out;
}
