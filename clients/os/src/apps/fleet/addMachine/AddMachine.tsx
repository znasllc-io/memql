import { useState, type FormEvent } from "react";
import { createWorkerToken } from "@znasllc-io/memql-sdk-core/identity";

import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { Button, Notice, Panel } from "../../../kit";
import {
  CLUSTER_URL_PLACEHOLDER,
  INSTALL_PLATFORMS,
  INSTALL_PLATFORM_LABEL,
  installCommand,
  workerClusterUrl,
  type InstallPlatform,
} from "./install";

// Add a machine: mint a worker token, show it once, and hand over the command
// that uses it.
//
// The custody reasoning below is copied from the portal's AddMachine.tsx on
// purpose. It is the part of this flow that is easiest to "fix" into
// something worse -- storing the token so it can be shown again, or reaching
// for the pairing-code endpoint -- and the argument against each has to
// travel with the code rather than living in a pull request nobody will read
// again.
//
// ===========================================================================
// THE TOKEN IS SHOWN ONCE AND THEN IT IS GONE
// ===========================================================================
// CreateWorkerTokenMsg returns the plain `mql_wkr_...` bearer in the reply and
// nothing keeps it: only its SHA-256 hash lands on the v1:identity:identity
// row. So this panel is the only moment it exists, and the copy says so
// rather than leaving an operator to discover it by closing the panel. If it
// is lost, the remedy is another token -- not a lookup, which does not exist.
//
// It is deliberately NOT written to localStorage, sessionStorage or a URL.
// This credential does not expire in fifteen minutes, it lets a machine act
// as its owner's worker, and a copy in browser storage outlives the tab. The
// OS persists a great deal to localStorage -- desks, surfaces, pins, these
// very settings -- which is exactly why the exception has to be stated: the
// habit of the codebase around it is to persist.
//
// ===========================================================================
// WHY THIS AND NOT identity's POST /pair/codes
// ===========================================================================
// The pairing-code flow exists and is the right shape for a worker that can
// run `memql worker pair` interactively -- the code is short, it is redeemed
// by the MACHINE, and the token never crosses the operator's screen. This
// application cannot start it: /pair/codes authenticates with
// `Authorization: Bearer <access token>`, and the OS's auth source keeps that
// bearer behind an interface no component may reach past (src/auth/source.ts).
// Punching a hole in that seam to save a copy-paste would be the wrong trade,
// and the portal declined the same trade for the same reason.
//
// CreateWorkerTokenMsg rides the connection's own credential, mints exactly
// the `--token` value the documented one-liner takes, and needs no new seam.
//
// ===========================================================================
// "IT WORKED" IS THE REGISTRATION APPEARING, NOT THE MINT SUCCEEDING
// ===========================================================================
// A minted token proves nothing about the machine -- the install can fail,
// the cluster URL can be wrong, the machine can be behind a firewall. So the
// panel watches the population it was opened from and reports when it GROWS,
// which is the subscription carrying a v1:worker:registration the cluster
// wrote. COUNTING rather than matching by name: the token's name is what the
// operator typed here and the registration's is the cockpit's hostname, so
// the two are routinely different and a name match would report failure on a
// success.

export function AddMachine({
  machineCount,
  onClose,
}: {
  /** The number of machines currently listed. Captured at mint time; the
   *  panel reports a registration when this grows past it. */
  machineCount: number;
  onClose: () => void;
}) {
  const connection = useOsConnection();
  const { config } = useSession();
  const [name, setName] = useState("");
  const [platform, setPlatform] = useState<InstallPlatform>("mac");
  const [computerUse, setComputerUse] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [token, setToken] = useState("");
  const [awaitingFrom, setAwaitingFrom] = useState<number | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const [copied, setCopied] = useState("");

  const clusterUrl = workerClusterUrl(config.domain);
  const registered = awaitingFrom !== null && machineCount > awaitingFrom;
  const command = installCommand({ platform, clusterUrl, token, computerUse });

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (connection === null || name.trim() === "" || busy) return;
    setBusy(true);
    setError("");
    setToken("");
    setAwaitingFrom(null);
    setAcknowledged(false);

    void createWorkerToken(connection.dispatcher, { name: name.trim() })
      .then((result) => {
        if (!result.success || result.plainToken === "") {
          setError(
            result.errorMessage ||
              result.errorCode ||
              "The cluster refused the mint and said nothing about why.",
          );
          return;
        }
        setToken(result.plainToken);
        setAwaitingFrom(machineCount);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setBusy(false));
  }

  // Best-effort. A clipboard that refuses (an insecure origin, a permission
  // the browser declined) leaves the text on screen and selectable, which is
  // why the failure is a caption rather than an error: nothing was lost.
  function copy(what: "token" | "command", text: string): void {
    const clipboard = globalThis.navigator?.clipboard;
    if (!clipboard) {
      setCopied("This browser did not offer a clipboard -- select the text and copy it.");
      return;
    }
    void clipboard
      .writeText(text)
      .then(() => setCopied(what === "token" ? "Token copied." : "Command copied."))
      .catch(() => setCopied("The browser refused the copy -- select the text and copy it."));
  }

  return (
    <Panel label="Add a machine">
      <form onSubmit={submit} className="os-form">
        {connection === null ? (
          <p className="os-caption">
            A token can only be minted over a live connection to the cluster.
          </p>
        ) : null}

        {error ? (
          <Notice
            tone="error"
            sentence="The token was not minted."
            next="Nothing was created; try again."
            detail={error}
          />
        ) : null}

        <div className="os-form-row">
          <label className="os-sr-only" htmlFor="fleet-add-name">
            What is this machine called
          </label>
          <input
            id="fleet-add-name"
            className="os-input"
            value={name}
            placeholder="studio-mac-mini"
            onChange={(e) => setName(e.target.value)}
          />
          <label className="os-select-label" htmlFor="fleet-add-platform">
            <span className="os-sr-only">Operating system</span>
            <select
              id="fleet-add-platform"
              className="os-select"
              value={platform}
              onChange={(e) => setPlatform(e.target.value as InstallPlatform)}
            >
              {INSTALL_PLATFORMS.map((one) => (
                <option key={one} value={one}>
                  {INSTALL_PLATFORM_LABEL[one]}
                </option>
              ))}
            </select>
          </label>
          <Button
            type="submit"
            tone="primary"
            busy={busy}
            busyLabel="Minting..."
            disabled={connection === null || name.trim() === ""}
          >
            Mint a token
          </Button>
        </div>

        {/* The build choice is an INPUT to the mint -- it decides which
            install command the panel then shows -- so it stays with the
            controls that produce it. */}
        <label className="os-check">
          <input
            type="checkbox"
            checked={computerUse}
            onChange={(e) => setComputerUse(e.target.checked)}
          />
          <span>
            Install the computer-use build (mouse, keyboard, screenshots). It asks for
            Accessibility and Screen Recording the first time it runs.
          </span>
        </label>

        <p className="os-caption">
          The name is yours, for the credential. The machine reports its own hostname when it
          connects, and you can rename it here afterwards.
        </p>
      </form>

      {token === "" ? null : (
        <div className="os-fleet-token">
          <Notice tone="warn">
            <p className="os-notice-line" role="alert">
              Copy this token now. It is not shown again -- the cluster keeps only its hash, so
              there is nowhere to look it up. If it is lost, mint another one and revoke this
              machine.
            </p>
          </Notice>

          <div className="os-fleet-secret">
            <code className="os-mono os-fleet-secret-value">{token}</code>
            <Button onClick={() => copy("token", token)} ariaLabel="Copy the worker token">
              Copy token
            </Button>
          </div>

          <p className="os-caption">Run this on {INSTALL_PLATFORM_LABEL[platform]}</p>
          <pre className="os-fleet-command os-mono">{command}</pre>
          <Button onClick={() => copy("command", command)} ariaLabel="Copy the install command">
            Copy command
          </Button>
          {copied ? <p className="os-caption">{copied}</p> : null}

          {clusterUrl === "" ? (
            <p className="os-caption">
              This deployment publishes no domain, so {CLUSTER_URL_PLACEHOLDER} is a placeholder
              -- substitute the address you reach this cluster's API at, with the scheme. A value
              with no scheme is dialled in the clear whatever its port.
            </p>
          ) : null}
          <p className="os-caption">
            The installer and the worker binary ship from the memql-cockpit repository -- the
            worker is a run mode of the <code>memql</code> command that repo builds, not something
            this engine builds. The full walkthrough, including the macOS permission prompts, is
            in the workers runbook.
          </p>

          <p role="status" className="os-status-line">
            {registered
              ? "A new machine has registered. It is in the list below."
              : "Waiting for the machine to connect. It appears in the list on its own the moment it registers -- this list is live, so there is nothing to reload."}
          </p>

          {/* The acknowledgment gate. Closing the panel is the moment the
              token stops being visible anywhere, so it is not a click that
              should be possible to make by accident while reading. */}
          <label className="os-check">
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={(e) => setAcknowledged(e.target.checked)}
            />
            <span>I have copied the token. I understand it cannot be shown again.</span>
          </label>
          <Button tone="primary" disabled={!acknowledged} onClick={onClose}>
            Done
          </Button>
        </div>
      )}

    </Panel>
  );
}
