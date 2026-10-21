import { localCockpitInstall } from "../localInstall";
import { Caption, CopyField, Notice } from "../../../../kit";
import type { Draft } from "../flow";
import {
  CLUSTER_URL_PLACEHOLDER,
  allowAppsCommand,
  installCommand,
  workerClusterUrl,
} from "../install";

// The command carries the token. Keep the separate token available on demand,
// and keep it only in this flow's memory: never in storage or a URL.

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
    workbench: draft.workbench,
    userLocal: draft.userLocal,
    localTest,
  });

  // ONE COMMAND AND ONE NOTE (memql#6214). The installer sets up local models
  // itself when the line carries --inference; if the runtime needs a person's
  // approval, the Model runtime check asks for the second command then, and
  // only then. The token and the app-permission recovery sit behind
  // disclosures: the command already carries the one, and the other is for
  // an older Cockpit.
  const note = [
    draft.userLocal ? "" : "Asks for your administrator password.",
    draft.computerUse && draft.platform === "mac" ? "Allow Accessibility and Screen Recording when asked." : "",
    localTest ? `Local test build ${localTest.version}.` : "",
    "Shown only here: it carries this machine’s token.",
  ].filter((part) => part !== "").join(" ");
  const allow = clusterUrl ? allowAppsCommand(clusterUrl, undefined, draft.userLocal) : "";

  return (
    <div className="os-stop-body os-fleet-addstop os-fleet-install">
      <div className="os-fleet-install-command">
        <CopyField value={command} label="the install command" id="fleet-add-command" />
        <Caption>{note}</Caption>
      </div>

      {clusterUrl === "" ? (
        <Notice tone="warn" sentence="Set the cluster address before running this command."
          next={`Replace ${CLUSTER_URL_PLACEHOLDER} with this cluster's API address, including https://.`} />
      ) : null}

      {allow === "" ? null : <details className="os-fleet-install-token">
        <summary>App permissions</summary>
        <div className="fleet-app-permission-body">
          <Caption>The installer allows Claude Code and Codex for this cluster; add <code>--no-apps</code> to skip that. If an app still says “Not allowed by Cockpit”, run:</Caption>
          <CopyField value={allow} label="the app permissions command" id="fleet-add-apps" />
        </div>
      </details>}

      <details className="os-fleet-install-token">
        <summary>Connection token</summary>
        <CopyField value={token} label="the worker token" id="fleet-add-token" />
      </details>
    </div>
  );
}
