import { useState, type FormEvent, type ReactNode } from "react";

import { ErrorMessage } from "../components/StatusMessage";
import { Badge, Band, Button, Callout, Select, TextInput } from "../ui";
import { AdminFrame, Reading, Refused } from "./AdminLayout";
import { surfaceById } from "./urls";
import { useAdminAccess } from "./useAdminConsole";
import {
  summarize,
  useProviderActions,
  useProviderStatus,
  type ProviderStatusRow,
} from "./useProviders";

// Settings -> AI providers (epic memql#4440, design D4).
//
// ===========================================================================
// THE PAGE'S JOB IS TO MAKE "NOT CONFIGURED" READ AS A CHOICE
// ===========================================================================
// After this epic, installing MemQL asks for no vendor key, so an operator's
// FIRST view of this page is an empty one -- every provider registered and
// none callable. That is a correctly installed cluster, and the copy has to
// say so. A red banner on a fresh install teaches the operator that something
// went wrong during setup, and the thing they will do about it is reinstall.
//
// ===========================================================================
// FEDERATION LEADS, AND THAT ORDERING IS THE RECOMMENDATION
// ===========================================================================
// Anthropic's block puts workload identity federation above the API key box,
// because the two are not equivalent options: federation leaves NO credential
// at rest anywhere -- each pod exchanges its own projected Kubernetes token
// for a one-hour bearer -- while a key sits in a row until somebody rotates
// it. A page that listed them side by side would be describing a preference
// it does not have. OpenAI is key-only and says why, with the slot visibly
// open rather than silently absent.
//
// ===========================================================================
// NOTHING HERE IS A GATE
// ===========================================================================
// All five constructs behind this page refuse below cluster owner in Go
// (component/memql/provider_auth_status_read.go, provider_verify.go,
// provider_config_write.go). What this file decides is what to OFFER. The tab
// is ABSENT rather than disabled for a non-owner (adminSurfacesFor), because a
// greyed-out tab advertises a capability whose only explanation is a refusal.

const VENDOR_LABELS: Record<string, string> = {
  anthropic: "Anthropic",
  openai: "OpenAI",
};

// AUTH_SOURCE_COPY is where the tier names become sentences. The distinction
// that matters most is env vs the two row tiers: a key in the pod's
// environment is the one source a portal write cannot change, so an operator
// saving a key and seeing no change needs to be told that, not left to guess.
const AUTH_SOURCE_COPY: Record<string, string> = {
  federation: "workload identity — no key at rest",
  globalSecret: "a sealed row in this cluster",
  globalVariable: "a plaintext row in this cluster",
  env: "this pod's environment — a saved key will not override it",
  unresolved: "nothing configured",
};

function sourceCopy(source: string): string {
  return AUTH_SOURCE_COPY[source] ?? source;
}

export function ProvidersPage(): ReactNode {
  const surface = surfaceById("providers");
  const { role, resolved } = useAdminAccess();
  // OWNER, not canAdminister. This is the one surface in the console with a
  // higher floor than the console itself.
  const isOwner = role === "owner";
  const status = useProviderStatus(isOwner);
  const actions = useProviderActions(status.reload);

  if (surface === undefined) return null;
  if (!isOwner) {
    return (
      <AdminFrame surface={surface} role={role} resolved={resolved}>
        <Refused role={role} resolved={resolved} ownerOnly />
      </AdminFrame>
    );
  }

  const summary = summarize(status.rows);

  return (
    <AdminFrame
      surface={surface}
      role={role}
      resolved={resolved}
      actions={
        <Button size="xs" onClick={status.reload} disabled={status.loading}>
          Refresh
        </Button>
      }
    >
      <Band>
        <div className="flex flex-wrap gap-2">
          <Reading
            label="Status"
            value={status.loading ? "…" : summary.headline}
            sub={
              summary.tone === "unconfigured"
                ? "installing spends no inference, so nothing was configured for you"
                : "as resolved by the node that answered this read"
            }
          />
        </div>
        {status.error === "" ? null : (
          <div className="mt-3">
            <ErrorMessage>Could not read provider status: {status.error}</ErrorMessage>
          </div>
        )}
        {actions.state.message === "" ? null : (
          <div className="mt-3">
            {actions.state.failed ? (
              <ErrorMessage>{actions.state.message}</ErrorMessage>
            ) : (
              <Callout tone="ok" title="Done">{actions.state.message}</Callout>
            )}
          </div>
        )}
      </Band>

      <Band
        title="What this cluster can call"
        meta="Per node: provider auth resolves once at boot, and this read was answered by one of them"
        panel
      >
        {status.rows.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {status.loading ? "Reading providers…" : "No providers are registered on this node."}
          </p>
        ) : (
          <ProviderTable rows={status.rows} busy={actions.state.busy} onVerify={actions.verify} />
        )}
      </Band>

      <Band
        title="Anthropic"
        meta="Federation is the recommended path: no key is stored anywhere"
      >
        <FederationForm
          busy={actions.state.busy}
          onSave={(fields) => void actions.saveFederation(fields)}
        />
        <div className="mt-6">
          <KeyForm
            vendor="anthropic"
            lede={
              "The local and development alternative. A key here is sealed with the cluster " +
              "master key and stored as a row; it is never read back into this page."
            }
            busy={actions.state.busy}
            onSave={(vendor, apiKey) => void actions.saveKey(vendor, apiKey)}
          />
        </div>
      </Band>

      <Band title="OpenAI" meta="Key only — OpenAI publishes no federation mechanism yet">
        <p className="mb-4 max-w-2xl text-sm text-muted">
          There is no workload-identity option to offer here. When OpenAI ships one, it belongs
          above this box for the same reason Anthropic&apos;s does — hopefully soon.
        </p>
        <KeyForm
          vendor="openai"
          lede="Sealed with the cluster master key and stored as a row. Never read back into this page."
          busy={actions.state.busy}
          onSave={(vendor, apiKey) => void actions.saveKey(vendor, apiKey)}
        />
      </Band>

      <Band
        title="Apply"
        meta="Saving stores the configuration; applying is what makes every node use it"
      >
        <p className="mb-3 max-w-2xl text-sm text-muted">
          Provider credentials resolve once, when a node starts. Apply tells every node in the
          cluster to re-resolve, so a key you just saved takes effect without a restart. It is a
          separate press on purpose: a mistyped key would otherwise take every provider on every
          node down at the moment you saved it. Each apply writes an audit event naming you.
        </p>
        <Button onClick={() => void actions.apply()} disabled={actions.state.busy}>
          {actions.state.busy ? "Working…" : "Apply to every node"}
        </Button>
      </Band>
    </AdminFrame>
  );
}

function ProviderTable({
  rows,
  busy,
  onVerify,
}: {
  rows: readonly ProviderStatusRow[];
  busy: boolean;
  onVerify: (provider: string) => Promise<void>;
}): ReactNode {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-sm">
        <thead className="text-xs tracking-wide text-muted uppercase">
          <tr>
            <th className="px-3 py-2">Provider</th>
            <th className="px-3 py-2">Vendor</th>
            <th className="px-3 py-2">Model</th>
            <th className="px-3 py-2">Credential</th>
            <th className="px-3 py-2">State</th>
            <th className="px-3 py-2" />
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.name} className="border-t border-line align-top">
              <td className="px-3 py-2 font-medium">{row.name}</td>
              <td className="px-3 py-2 text-muted">{VENDOR_LABELS[row.vendor.toLowerCase()] ?? row.vendor}</td>
              <td className="px-3 py-2 text-muted">{row.model === "" ? "—" : row.model}</td>
              <td className="px-3 py-2 text-muted">{sourceCopy(row.authSource)}</td>
              <td className="px-3 py-2">
                {row.available ? (
                  <Badge tone="ok">callable</Badge>
                ) : (
                  <>
                    <Badge tone="neutral">not configured</Badge>
                    {/* The reason, verbatim from the constructor that refused
                        -- it already names the missing variable or the
                        half-configured federation set, and a friendlier
                        rewrite here would be a vaguer account of a decision
                        made somewhere else. */}
                    {row.reason === "" ? null : (
                      <p className="mt-1 max-w-md text-xs text-subtle">{row.reason}</p>
                    )}
                  </>
                )}
              </td>
              <td className="px-3 py-2">
                {/* Offered only for something callable: verifying an
                    unconfigured provider asks the vendor a question about a
                    credential that was never sent. */}
                {row.available ? (
                  <Button size="xs" disabled={busy} onClick={() => void onVerify(row.name)}>
                    Verify
                  </Button>
                ) : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// The four federation ids, plus the optional workspace.
//
// ALL-OR-NONE IS STATED INLINE rather than only enforced. The engine refuses a
// partial write (and a partial config refuses BOOT), so an operator who fills
// two boxes and saves gets a refusal — which is the right outcome and a
// baffling one if the page never said the rule.
const FEDERATION_FIELDS: readonly { id: string; label: string; hint: string }[] = [
  { id: "ruleId", label: "Federation rule id", hint: "fdrl_… — created in the Anthropic Console" },
  { id: "organizationId", label: "Organization id", hint: "the UUID the rule belongs to" },
  { id: "serviceAccountId", label: "Service account id", hint: "the account the rule maps this cluster onto" },
  {
    id: "identityTokenFile",
    label: "Identity token file",
    hint: "path to the pod's projected token, mounted for the Anthropic audience",
  },
];

const WORKSPACE_FIELD = {
  id: "workspaceId",
  label: "Workspace id (optional)",
  hint: "only needed when the rule spans more than one workspace",
};

function FederationForm({
  busy,
  onSave,
}: {
  busy: boolean;
  onSave: (fields: Record<string, string>) => void;
}): ReactNode {
  const [draft, setDraft] = useState<Record<string, string>>({});
  const set = (id: string, value: string) => setDraft((d) => ({ ...d, [id]: value }));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    onSave(draft);
  };

  return (
    <form onSubmit={submit} className="rounded-lg border border-line bg-surface p-4">
      <p className="mb-4 max-w-2xl text-sm text-muted">
        With federation, no API key exists anywhere: each pod presents its own projected Kubernetes
        token and Anthropic exchanges it for a bearer that lasts an hour. All four ids below are
        required together — a half-configured set refuses to boot rather than quietly falling back
        to a key, so this page refuses it here instead.{" "}
        <a className="underline" href="https://github.com/znasllc-io/memql/blob/main/docs/public/operate/auth/anthropic-federation.md">
          The setup runbook
        </a>{" "}
        walks through the Console side.
      </p>
      <div className="grid gap-3 lg:grid-cols-2">
        {[...FEDERATION_FIELDS, WORKSPACE_FIELD].map((field) => (
          <label key={field.id} className="flex flex-col gap-1 text-xs text-muted">
            {field.label}
            <TextInput value={draft[field.id] ?? ""} onChange={(next) => set(field.id, next)} />
            <span className="text-subtle">{field.hint}</span>
          </label>
        ))}
      </div>
      <div className="mt-4">
        <Button type="submit" disabled={busy}>
          Save federation settings
        </Button>
      </div>
    </form>
  );
}

// The key box.
//
// WRITE-ONLY, AND THE PAGE CANNOT DO OTHERWISE: no construct in this epic
// reads a key back, so there is nothing for this form to prefill even if it
// wanted to. It clears the field on submit, so a key does not sit in a DOM
// node behind a page an operator walked away from.
function KeyForm({
  vendor,
  lede,
  busy,
  onSave,
}: {
  vendor: string;
  lede: string;
  busy: boolean;
  onSave: (vendor: string, apiKey: string) => void;
}): ReactNode {
  const [value, setValue] = useState("");
  const [chosen, setChosen] = useState(vendor);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (value.trim() === "") return;
    onSave(chosen, value);
    setValue("");
  };

  return (
    <form onSubmit={submit} className="rounded-lg border border-line bg-surface p-4">
      <p className="mb-3 max-w-2xl text-sm text-muted">{lede}</p>
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-xs text-muted">
          Vendor
          <Select ariaLabel={`Vendor for the ${vendor} key`} value={chosen} onChange={setChosen}>
            {Object.entries(VENDOR_LABELS).map(([id, label]) => (
              <option key={id} value={id}>
                {label}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex min-w-[20rem] flex-1 flex-col gap-1 text-xs text-muted">
          API key
          {/* type=password so a key does not sit legible on a screen an
              operator is sharing. It is not a security control -- the value is
              in the DOM either way -- and the real protections are that the
              field is cleared on submit and that nothing ever reads a key
              back. */}
          <TextInput type="password" value={value} onChange={setValue} />
          <span className="text-subtle">
            Stored sealed. This page shows a fingerprint afterwards, never the value.
          </span>
        </label>
        <Button type="submit" disabled={busy || value.trim() === ""}>
          Save key
        </Button>
      </div>
    </form>
  );
}
