import { localCockpitInstall } from "../localInstall";
import { Caption, CopyField, Notice, Subhead } from "../../../../kit";
import type { Draft } from "../flow";
import {
  CLUSTER_URL_PLACEHOLDER,
  setupCommand,
  installCommand,
  workerClusterUrl,
} from "../install";

// The command carries the token. Keep the separate token available on demand,
// and keep it only in this flow's memory: never in storage or a URL.
// Runtime installation needs an interactive approval, so local models remain
// a second command, run after the installer finishes.

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
  const localTest = draft.platform === "mac" ? localCockpitInstall(domain) : null;
  const command = installCommand({
    platform: draft.platform,
    clusterUrl,
    token,
    computerUse: draft.computerUse,
    inference: draft.inference,
    userLocal: draft.userLocal,
    localTest,
  });

  return (
    <div className="os-stop-body os-fleet-addstop os-fleet-install">
      <div className="os-fleet-install-command">
        <Subhead>{draft.inference ? "1. Install MemQL Cockpit" : "Install MemQL Cockpit"}</Subhead>
        <CopyField value={command} label="the install command" id="fleet-add-command" />
        <Caption>
          {draft.userLocal ? "" : "Enter your administrator password when asked. "}
          {draft.computerUse && draft.platform === "mac" ? "Allow Accessibility and Screen Recording when prompted. " : ""}
          {localTest ? `Local test build ${localTest.version}. Use this Mac. ` : ""}
          Wait for SUCCESS before closing the terminal.
        </Caption>
      </div>

      {clusterUrl === "" ? (
        <Notice tone="warn" sentence="Set the cluster address before running this command."
          next={`Replace ${CLUSTER_URL_PLACEHOLDER} with this cluster's API address, including https://.`} />
      ) : null}

      {draft.inference ? (
        <div className="os-fleet-install-command">
          <Subhead>2. Set up local models</Subhead>
          <CopyField value={setupCommand(draft.userLocal, true)} label="the local models setup command" id="fleet-add-inference" />
          <Caption>
            Run once the installer prints SUCCESS. Approve the runtime setup to download the recommended models.
          </Caption>
        </div>
      ) : null}

      <Caption>Keep this page open until the machine connects. The install command contains a private token shown only during this setup.</Caption>
      <details className="os-fleet-install-token">
        <summary>Connection token</summary>
        <CopyField value={token} label="the worker token" id="fleet-add-token" />
      </details>
    </div>
  );
}
