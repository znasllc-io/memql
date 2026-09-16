import { localCockpitInstall } from "../localInstall";
import { Caption, CopyField, Notice, Subhead } from "../../../../kit";
import { installSteps, type Draft } from "../flow";
import {
  CLUSTER_URL_PLACEHOLDER,
  setupCommand,
  INSTALL_PLATFORM_LABEL,
  installCommand,
  workerClusterUrl,
} from "../install";

// INSTALL: one line, on the machine itself -- and what happens when it is
// pasted, in the order it happens (design record D5, D13, D15).
//
// ===========================================================================
// THE TOKEN IS SHOWN ONCE AND THEN IT IS GONE
// ===========================================================================
// CreateWorkerTokenMsg returns the plain `mql_wkr_...` bearer in the reply
// and nothing keeps it: only its SHA-256 hash lands on the identity row. So
// this stop is the only place it exists, and the copy says so rather than
// leaving a person to discover it by closing the window. It is deliberately
// NOT written to localStorage, sessionStorage or a URL -- see
// useAddMachineFlow.ts, where the reason and the consequence live.
//
// ===========================================================================
// THE STEPS ARE NUMBERED BECAUSE THEY ARE A SEQUENCE
// ===========================================================================
// A terminal, a paste, a password prompt, a permission dialog, a download:
// each happens after the one before, on the machine, and the person reading
// this is about to walk away from this screen to do them. Numbers are right
// here and nowhere else on the page.
//
// ===========================================================================
// LOCAL MODELS ARE A SECOND COMMAND, SAID UP FRONT (D13)
// ===========================================================================
// The one-liner runs without a terminal to ask on, so it cannot approve a
// runtime install; on a fresh machine `worker setup --inference` is always
// run by hand afterwards. Saying so here, beside the line, is what stops it
// reading as a failure when the installer prints it.

export function InstallStop({
  draft,
  token,
  domain,
}: {
  draft: Draft;
  token: string;
  /** The cluster's published domain, or "". */
  domain: string;
}) {
  const clusterUrl = workerClusterUrl(domain);
  const command = installCommand({
    platform: draft.platform,
    clusterUrl,
    token,
    computerUse: draft.computerUse,
    inference: draft.inference,
    userLocal: draft.userLocal,
    localTest: localCockpitInstall(domain),
  });

  return (
    <div className="os-stop-body os-fleet-addstop">
      <Notice tone="warn">
        <p className="os-notice-line" role="alert">
          Keep this page open until the machine connects. The token is shown only once.
          If you lose it, cancel this setup and create a new token.
        </p>
      </Notice>

      <Subhead>Token</Subhead>
      <CopyField value={token} label="the worker token" id="fleet-add-token" />

      <Subhead>Run this on {INSTALL_PLATFORM_LABEL[draft.platform]}</Subhead>
      <CopyField value={command} label="the install command" id="fleet-add-command" />
      {draft.platform === "mac" && localCockpitInstall(domain) ? <Caption>Local test build {localCockpitInstall(domain)?.version}. The installer opens the native permission guide on this Mac.</Caption> : null}
      <Caption>Connects this computer to your cluster and keeps any existing cluster connections.</Caption>

      {clusterUrl === "" ? (
        <Caption>
          This deployment publishes no domain, so {CLUSTER_URL_PLACEHOLDER} is a placeholder --
          substitute the address you reach this cluster's API at, with the scheme. A value with no
          scheme is dialled in the clear whatever its port.
        </Caption>
      ) : null}

      <ol className="os-fleet-steps" aria-label="What happens on the machine">
        {installSteps(draft).map((step) => (
          <li key={step}>{step}</li>
        ))}
      </ol>

      {draft.inference ? (
        <>
          <Subhead>Then, for local models</Subhead>
          <CopyField value={setupCommand(draft.userLocal, true)} label="the local models setup command" id="fleet-add-inference" />
          <Caption>
            Run it in the same terminal once the installer prints SUCCESS. It checks the hardware,
            shows the runtime installation commands for approval and downloads the recommended
            models.{draft.platform === "linux" ? " On Linux, the default runtime runs as a service for your account; Docker is optional." : ""}
            {" "}The checks below update automatically as models become available.
          </Caption>
        </>
      ) : null}

    </div>
  );
}
