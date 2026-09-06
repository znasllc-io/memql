import { useState } from "react";

import {
  Button,
  Caption,
  Chip,
  Chips,
  Field,
  Head,
  Input,
  Notice,
  Panel,
  Subhead,
} from "../../kit";
import { useSession } from "../../chrome/access";
import type { RoleRequirement } from "../../system/roles";
import {
  sourceCopy,
  summarize,
  useProviderActions,
  useProviderRegistry,
  vendorLabel,
} from "./providerFacts";

// AI providers (epic memql#4984): which models this cluster can call, and how
// it authenticates to call them.
//
// OWNER ONLY. `providerAuthStatus`, `providerKeySet`, `providerFederationSet`
// and `providersReload` are all owner-gated in the engine; a section offered
// to admin would be a page whose every control answers with a refusal. The
// manifest declares it and this constant is the one copy of the value.
//
// FIVE RULES, and they are the design of this surface:
//
//  1. A SECRET IS WRITE-ONLY IN BOTH DIRECTIONS. The field POSTS a value and
//     nothing ever renders one back -- not masked, not truncated, not dots.
//     The plaintext crosses the wire once, is sealed server-side under a key
//     that must never exist in a browser, and no reply carries it again. What
//     comes back is a FINGERPRINT, which is the only thing about the value
//     this surface can honestly say. The field is not a password box for the
//     same reason Integrations' is not: there is nothing to hide from the
//     person typing, and masking a 100-character key stops them checking it.
//  2. UNCONFIGURED IS A NORMAL STATE. Installing spends no inference and asks
//     for no key, so every provider registers unavailable on a fresh cluster.
//     That is the opening state and it reads as a next step, never a fault --
//     an operator who meets a red banner on a fresh install concludes the
//     install failed.
//  3. FEDERATION LEADS, AND THE ORDER IS THE RECOMMENDATION. For Anthropic the
//     two paths are not equivalent options: federation leaves NO credential at
//     rest anywhere, because each pod exchanges its own projected Kubernetes
//     token for a one-hour bearer. It goes above the key box and says so.
//  4. VERIFY REACHES A VENDOR, SO A PERSON PRESSES IT. A live credential check
//     spends somebody's quota with a third party. It is never something a
//     panel does on render, and a refusal from the vendor is a RESULT rendered
//     in the vendor's own words -- not an exception blamed on this console.
//  5. A SAVE IS NOT AN APPLY. Saving writes the row; the registry each node
//     resolved at boot does not move until Apply broadcasts. Two controls,
//     because they are two facts, and an operator who saved a key and saw
//     nothing change has to be told which one they still owe.

/** The section's role floor. Presentation only; every gate is server-side. */
export const PROVIDERS_SECTION_ROLE: RoleRequirement = { min: "owner" };

/** The four federation ids, plus the optional workspace. */
const FEDERATION_FIELDS = [
  { key: "ruleId", label: "Federation rule id", hint: "fdrl_...", required: true },
  { key: "organizationId", label: "Organization id", hint: "UUID", required: true },
  { key: "serviceAccountId", label: "Service account id", hint: "", required: true },
  {
    key: "identityTokenFile",
    label: "Projected token path",
    hint: "/var/run/secrets/...",
    required: true,
  },
  {
    key: "workspaceId",
    label: "Workspace id",
    hint: "Only for a rule spanning more than one workspace",
    required: false,
  },
] as const;

export function ProvidersSection() {
  const { access } = useSession();
  const registry = useProviderRegistry(true);
  const actions = useProviderActions(registry.reload);
  const summary = summarize(registry.rows);

  return (
    <div className="os-settings">
      <Head title="AI providers" meta={registry.rows.length === 0 ? undefined : `${registry.rows.length} registered`}>
        <Button
          tone="primary"
          onClick={() => void actions.apply()}
          busy={actions.state.busy}
          busyLabel="Applying"
        >
          Apply
        </Button>
      </Head>
      <p className="os-caption">
        What this cluster can call, and how it proves it may. A save stores the
        credential; Apply is what makes every node re-resolve its registry.
      </p>

      {registry.error ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.clusterRole || "your role"}.`}
          detail={registry.error}
        />
      ) : (
        <Notice
          tone={summary.tone === "partial" ? "warn" : "info"}
          sentence={summary.headline}
          next={
            summary.tone === "unconfigured"
              ? "Configure one below, then Apply."
              : undefined
          }
        />
      )}

      {actions.state.message ? (
        <Notice
          tone={actions.state.failed ? "error" : "info"}
          sentence={actions.state.failed ? "That did not go through." : "Done."}
          detail={actions.state.message}
        />
      ) : null}

      <AnthropicPanel
        busy={actions.state.busy}
        onSaveKey={(key) => void actions.saveKey("anthropic", key)}
        onSaveFederation={(fields) => void actions.saveFederation(fields)}
      />

      <OpenAiPanel busy={actions.state.busy} onSaveKey={(key) => void actions.saveKey("openai", key)} />

      <Panel label="What this node can call">
        <Subhead>What this node can call</Subhead>
        {registry.rows.length === 0 ? (
          <Caption>
            {registry.loading
              ? "Reading the registry"
              : "Nothing is registered on the node that answered. Save a credential above, then Apply."}
          </Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="Registered providers">
            {registry.rows.map((p) => (
              <li key={p.name}>
                <span
                  className="os-dot"
                  data-os-dot={p.available ? "reachable" : "unreachable"}
                  role="img"
                  aria-label={p.available ? "can be called" : "cannot be called"}
                />{" "}
                <span className="os-mono">{p.name}</span> -- {vendorLabel(p.vendor)} {p.model},
                credential from {sourceCopy(p.authSource)}
                {p.reason ? ` -- ${p.reason}` : ""}{" "}
                <Button
                  onClick={() => void actions.verify(p.name)}
                  busy={actions.state.busy}
                  busyLabel="Asking"
                  ariaLabel={`Verify ${p.name} with the vendor`}
                >
                  Verify
                </Button>
              </li>
            ))}
          </ul>
        )}
        <div className="os-refresh-row">
          <Button onClick={registry.reload} busy={registry.loading} busyLabel="Reading">
            Refresh
          </Button>
          <Caption>
            {registry.fetchedAt === null
              ? ""
              : `Read at ${new Date(registry.fetchedAt).toISOString()}. `}
            One node&apos;s own registry -- which replica answered is not
            knowable from here, which is why Apply broadcasts rather than
            relying on repeated reads.
          </Caption>
        </div>
      </Panel>
    </div>
  );
}

function AnthropicPanel({
  busy,
  onSaveKey,
  onSaveFederation,
}: {
  busy: boolean;
  onSaveKey: (key: string) => void;
  onSaveFederation: (fields: Record<string, string>) => void;
}) {
  const [fed, setFed] = useState<Record<string, string>>({});
  const [key, setKey] = useState("");

  // ALL-OR-NONE, checked here as well as server-side, and the reason is worth
  // knowing: a partial federation set REFUSES BOOT. The engine enforces it
  // before the write so a half-set cannot be stored; refusing here too means
  // the person is told which field is missing while they are still looking at
  // it, rather than after a round trip.
  const complete = FEDERATION_FIELDS.filter((f) => f.required).every(
    (f) => (fed[f.key] ?? "").trim() !== "",
  );

  return (
    <Panel label="Anthropic">
      <Subhead>Anthropic</Subhead>
      <Chips label="Anthropic credential paths">
        <Chip tone="accent">Federation recommended</Chip>
        <Chip tone="muted">Write-only</Chip>
      </Chips>
      <p className="os-caption">
        With workload identity federation no API key exists anywhere: each pod
        presents its own projected Kubernetes token and the SDK exchanges it
        for a one-hour bearer. All four ids or none -- a partial set refuses
        boot rather than falling back to a key.
      </p>
      {FEDERATION_FIELDS.map((f) => (
        <Field key={f.key} label={f.required ? f.label : `${f.label} (optional)`}>
          <Input
            id={`provider-fed-${f.key}`}
            label={f.label}
            value={fed[f.key] ?? ""}
            onChange={(next) => setFed((held) => ({ ...held, [f.key]: next }))}
            placeholder={f.hint || undefined}
          />
        </Field>
      ))}
      <div className="os-refresh-row">
        <Button
          tone="primary"
          onClick={() => onSaveFederation(fed)}
          disabled={!complete}
          busy={busy}
          busyLabel="Saving"
        >
          Save federation
        </Button>
        <Caption>
          {complete
            ? "Stored as plaintext rows. None of these five is a credential."
            : "Every id but the workspace is needed before this can be saved."}
        </Caption>
      </div>

      <Field label="API key">
        <Input
          id="provider-key-anthropic"
          label="Anthropic API key (new value)"
          value={key}
          onChange={setKey}
          placeholder="Replace this credential"
          onEnter={() => {
            onSaveKey(key);
            setKey("");
          }}
        />
        <Button
          onClick={() => {
            onSaveKey(key);
            setKey("");
          }}
          disabled={key.trim() === ""}
          busy={busy}
          busyLabel="Sealing"
        >
          Seal key
        </Button>
      </Field>
      <Caption>
        The alternative to federation, not a companion to it. The value is
        sealed in the cluster and never sent back -- what returns is a
        fingerprint.
      </Caption>
    </Panel>
  );
}

function OpenAiPanel({ busy, onSaveKey }: { busy: boolean; onSaveKey: (key: string) => void }) {
  const [key, setKey] = useState("");
  return (
    <Panel label="OpenAI">
      <Subhead>OpenAI</Subhead>
      <Chips label="OpenAI credential paths">
        <Chip tone="muted">Write-only</Chip>
      </Chips>
      <p className="os-caption">
        A key is the only path here -- OpenAI publishes no federation mechanism
        yet, so the absence of that block is theirs rather than an omission.
      </p>
      <Field label="API key">
        <Input
          id="provider-key-openai"
          label="OpenAI API key (new value)"
          value={key}
          onChange={setKey}
          placeholder="Replace this credential"
          onEnter={() => {
            onSaveKey(key);
            setKey("");
          }}
        />
        <Button
          tone="primary"
          onClick={() => {
            onSaveKey(key);
            setKey("");
          }}
          disabled={key.trim() === ""}
          busy={busy}
          busyLabel="Sealing"
        >
          Seal key
        </Button>
      </Field>
      <Caption>
        Sealed in the cluster and never sent back. A key set in this pod&apos;s
        environment outranks a stored one, and the registry above says which
        source each provider actually resolved.
      </Caption>
    </Panel>
  );
}
