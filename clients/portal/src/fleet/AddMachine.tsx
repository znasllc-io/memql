import { useState, type FormEvent, type ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import {
  Button,
  Callout,
  Checkbox,
  DataText,
  Field,
  FormActions,
  FormRow,
  Panel,
  Select,
  TextInput,
} from "../ui";
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
// ===========================================================================
// THE TOKEN IS SHOWN ONCE AND THEN IT IS GONE
// ===========================================================================
// CreateWorkerTokenMsg returns the plain `mql_wkr_...` bearer in the reply and
// nothing keeps it: only its SHA-256 hash lands on the v1:identity:identity
// row. So this panel is the only moment it exists, and the copy says so rather
// than leaving an operator to discover it by closing the panel. If it is lost,
// the remedy is another token -- not a lookup, which does not exist.
//
// It is deliberately NOT written to localStorage or a URL. The whole custody
// argument for the split in src/auth/identityAuthSource.ts applies here with
// more force: this credential does not expire in fifteen minutes, it lets a
// machine act as its owner's worker, and a copy in browser storage outlives
// the tab.
//
// ===========================================================================
// WHY THIS AND NOT identity's POST /pair/codes
// ===========================================================================
// The pairing-code flow exists and is the right shape for a worker that can
// run `memql-cockpit worker pair` interactively -- the code is short, it is
// redeemed by the MACHINE, and the token never crosses the operator's screen.
// The portal cannot start it: /pair/codes authenticates with
// `Authorization: Bearer <access token>`, and this application deliberately has
// no way to read that token (src/cluster/auth.ts: the credential lives in a
// closure and no component may reach past the interface for it). Punching a
// hole in that seam to save a copy-paste would be the wrong trade.
//
// CreateWorkerTokenMsg rides the connection's own credential, mints exactly the
// `--token` value the documented one-liner takes, and needs no new seam.
//
// ===========================================================================
// "IT WORKED" IS THE REGISTRATION APPEARING, NOT THE MINT SUCCEEDING
// ===========================================================================
// A minted token proves nothing about the machine -- the install can fail, the
// cluster URL can be wrong, the machine can be behind a firewall. So the panel
// watches the population it was opened from and reports when it GROWS, which
// is the subscription carrying a v1:worker:registration the cluster wrote.
// Counting rather than matching by name: the token's name is what the operator
// typed here, and the registration's is the cockpit's hostname, so the two are
// routinely different and a name match would report failure on a success.

export function AddMachine({
  domain,
  machineCount,
}: {
  // The cluster's own domain (runtime config), used to compose the cluster URL
  // the worker dials. Empty renders a placeholder rather than half a URL.
  domain: string;
  // The number of machines currently listed. Captured at mint time; the panel
  // reports a registration when this grows past it.
  machineCount: number;
}): ReactNode {
  const { clients } = useCluster();
  const [name, setName] = useState("");
  const [platform, setPlatform] = useState<InstallPlatform>("mac");
  const [computerUse, setComputerUse] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [token, setToken] = useState("");
  const [awaitingFrom, setAwaitingFrom] = useState<number | null>(null);

  const clusterUrl = workerClusterUrl(domain);
  const registered = awaitingFrom !== null && machineCount > awaitingFrom;

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (clients === null || name.trim() === "" || busy) return;
    setBusy(true);
    setError("");
    setToken("");
    setAwaitingFrom(null);

    void clients
      .createWorkerToken({ name: name.trim() })
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

  return (
    <Panel>
      <form onSubmit={submit} className="flex flex-col gap-3">
        {clients === null ? (
          <Callout tone="warn" title="Not connected">
            A token can only be minted over a live connection to the cluster.
          </Callout>
        ) : null}

        {error === "" ? null : (
          <Callout tone="danger" title="The token was not minted">
            {error}
          </Callout>
        )}

        {/* The build choice is an INPUT to the mint -- it decides which install
            command the next screen shows -- so it stays ahead of the button
            that acts on it. Bringing the button up onto the control line (the
            alignment this issue is about) would otherwise have left an option
            sitting after the submit it changes. */}
        <Checkbox
          checked={computerUse}
          onChange={setComputerUse}
          label="Install the computer-use build (mouse, keyboard, screenshots). It asks for Accessibility and Screen Recording the first time it runs."
        />

        <FormRow>
          <Field
            label="What is this machine called"
            grow
            hint="Yours, for the credential. The machine reports its own hostname when it connects, and you can rename it here afterwards."
          >
            <TextInput value={name} onChange={setName} placeholder="jose-mac-mini" />
          </Field>
          <Field label="Operating system">
            <Select
              value={platform}
              onChange={(next) => setPlatform(next as InstallPlatform)}
              ariaLabel="Operating system"
            >
              {INSTALL_PLATFORMS.map((one) => (
                <option key={one} value={one}>
                  {INSTALL_PLATFORM_LABEL[one]}
                </option>
              ))}
            </Select>
          </Field>
          <FormActions>
            <Button
              type="submit"
              tone="primary"
              busy={busy}
              busyLabel="Minting…"
              disabled={clients === null || name.trim() === ""}
            >
              Mint a token
            </Button>
          </FormActions>
        </FormRow>
      </form>

      {token === "" ? null : (
        <div className="mt-4 flex flex-col gap-3 border-t border-line pt-4">
          <Callout tone="warn" title="Copy this token now. It is not shown again.">
            The cluster keeps only its hash, so there is nowhere to look it up. If it is lost,
            mint another one and revoke this machine.
          </Callout>

          <div>
            <p className="text-xs font-medium text-muted">Worker token</p>
            <p className="mt-1">
              <DataText kind="id">{token}</DataText>
            </p>
          </div>

          <div>
            <p className="text-xs font-medium text-muted">
              Run this on {INSTALL_PLATFORM_LABEL[platform]}
            </p>
            <pre className="mt-1 overflow-x-auto rounded border border-line bg-surface p-3 font-mono text-xs text-fg">
              {installCommand({ platform, clusterUrl, token, computerUse })}
            </pre>
            {clusterUrl === "" ? (
              <p className="mt-1 text-xs text-subtle">
                This deployment publishes no domain, so {CLUSTER_URL_PLACEHOLDER} is a
                placeholder -- substitute the address you reach this cluster's API at, with the
                scheme. A value with no scheme is dialled in the clear whatever its port.
              </p>
            ) : null}
            <p className="mt-1 text-xs text-subtle">
              The installer and the worker binary ship from the memql-cockpit repository -- the
              worker is a run mode of the Cockpit binary, not something this engine builds. The
              full walkthrough, including the macOS permission prompts, is in the workers
              runbook.
            </p>
          </div>

          <p role="status" className="text-sm">
            {registered ? (
              <span className="text-fg">
                A new machine has registered. It is in the list below.
              </span>
            ) : (
              <span className="text-muted">
                Waiting for the machine to connect. It appears below on its own the moment it
                registers -- this page is live, so there is nothing to reload.
              </span>
            )}
          </p>
        </div>
      )}
    </Panel>
  );
}
