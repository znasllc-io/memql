import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useEffect, useState } from "react";

import { Button, Caption, RecordList, RecordRow, Field, Head, Input, Notice, Panel, Subhead } from "../../kit";
import { findRegion, revealRegion } from "../../kit";
import { useSession } from "../../chrome/access";
import { RoutingIsInFleet } from "./RoutingLink";
import type { OsAppProps } from "../../system/registry";
import {
  doorFor,
  federationFields,
  localityOf,
  missingFederationFields,
  sourceCopy,
  summarize,
  useProviderActions,
  useProviderRegistry,
  vendorLabel,
  type IssuerReach,
  type VendorDoor,
} from "./providerFacts";
import { doorReadings, useInferenceStatus, type DoorId, type DoorReading } from "./routingFacts";

// Settings -> Vendors (was Doors; routing redesign, 2026-09-28). Settings keeps
// what is a CREDENTIAL or a cluster-wide VALUE, and a vendor's federation ids
// are exactly that -- so this page is the two vendors, their forms, Apply, and
// what this node can call. Which source a call tries, in what order, is Fleet
// > Routing's, and the page says so once, quietly.
//
// ===========================================================================
// THE FIVE RULES CARRIED OVER FROM "AI PROVIDERS", UNCHANGED
// ===========================================================================
//  1. THE STATE IS A COLUMN, AND IT IS THE CONTENT. Every vendor's state word
//     sits in one column so the page is read by scanning it.
//  2. AN UNSET VENDOR IS THE QUIETEST THING ON THE SCREEN. A fresh cluster has
//     none and every local cluster has none permanently; a warning banner on a
//     fresh install reads as a failed install.
//  3. HALF-CONFIGURED IS THE ONE THAT SHOUTS, because the engine REFUSES BOOT
//     on it -- hours after the save that caused it -- and it renders the
//     engine's own sentence.
//  4. A LOCAL CLUSTER IS TOLD, NOT ASKED. Its OIDC issuer is private, so no id
//     anybody types will ever work; the form is ABSENT with a sentence in its
//     place (rule 12: an act that is not legal is absent).
//  5. VERIFY REACHES A VENDOR, SO A PERSON PRESSES IT. Never on render; a
//     refusal is a RESULT in the vendor's own words.
//
// AND A SAVE IS STILL NOT AN APPLY. Saving writes the ids; the registry each
// node resolved at boot does not move until Apply broadcasts.

/**
 * The section's role requirement (D7): OWNER OR DEVELOPER, AS A SET,
 * EXPLICITLY NOT ADMIN. A developer helps an owner through setup; an admin's
 * concern is user administration, and the ladder puts admin below developer,
 * so a minimum cannot express it. Presentation only; every gate is
 * server-side.
 */
export const VENDORS_SECTION_RESOURCE = "app:settings/providers";

/**
 * The state words, in one place. `closed` and `unset` are the SAME state
 * wearing two words: "Not set up" invites somebody to set it up, which on a
 * local cluster is an invitation to waste an afternoon.
 */
export const VENDOR_WORDS = {
  open: "Set up",
  unset: "Not set up",
  closed: "Not available",
  half: "Half set up",
  unknown: "Not read",
} as const;

const VENDOR_IDS: readonly DoorId[] = ["anthropic", "openai"];

export function VendorsSection({
  intent,
  consumeIntent,
}: {
  intent?: OsAppProps["intent"];
  consumeIntent?: OsAppProps["consumeIntent"];
} = {}) {
  const { access, config } = useSession();
  const registry = useProviderRegistry(true);
  const inference = useInferenceStatus(true);
  const actions = useProviderActions(registry.reload);
  const reach = localityOf(config.domain);

  // WHICH VENDOR FORM IS OPEN. Empty means none, which is the standing state.
  const [openVendor, setOpenVendor] = useState<string>("");

  const vendors = doorReadings(inference.status, (vendor) => doorFor(vendor, registry.rows)).filter((d) => VENDOR_IDS.includes(d.id));

  // ARRIVING AT A VENDOR (epic memql#5106). TWO EFFECTS, NOT ONE: the panel is
  // opened on demand, so on the tick the intent arrives its region is not in
  // the DOM yet. The first opens it; the second, once it exists, reveals it
  // and consumes the intent -- so the instruction is not eaten by a render
  // that could not act on it.
  const vendor = typeof intent?.payload["vendor"] === "string" ? intent.payload["vendor"] : "";
  const wanted = VENDOR_IDS.includes(vendor as DoorId) ? vendor : "";
  useEffect(() => {
    if (!intent || vendor === "") return;
    if (wanted === "") {
      // A vendor this page does not serve: reveal nothing, but CONSUME it.
      consumeIntent?.(intent.id);
      return;
    }
    setOpenVendor(wanted);
  }, [intent, vendor, wanted, consumeIntent]);

  useEffect(() => {
    if (!intent || wanted === "" || openVendor !== wanted) return;
    revealRegion(findRegion("os-vendor", wanted));
    consumeIntent?.(intent.id);
  }, [intent, wanted, openVendor, consumeIntent]);

  return (
    <div className="os-settings">
      <Head title="Vendors" meta={registry.fetchedAt !== null && !registry.error ? vendors.length : undefined}>
        <Button tone="primary" onClick={() => void actions.apply()} busy={actions.state.busy} busyLabel="Applying">
          Apply
        </Button>
      </Head>
      <p className="os-caption">
        Ids from each vendor&apos;s own console. There is no API key to enter,
        here or anywhere else. A vendor bills per call.
      </p>
      <RoutingIsInFleet tab="routes" />

      {registry.error ? (
        <Notice tone="warn" sentence={`The cluster declined this read for ${access?.role || "your role"}.`} detail={registry.error} />
      ) : null}

      {actions.state.message ? (
        <Notice tone={actions.state.failed ? "error" : "info"} sentence={actions.state.failed ? "That did not go through." : "Done."} detail={actions.state.message} />
      ) : null}

      <RecordList as="ul" label="Vendors">
        {vendors.map((door) => (
          <VendorRow key={door.id} door={door} reach={reach} open={openVendor === door.id} onOpenVendor={() => setOpenVendor(openVendor === door.id ? "" : door.id)} />
        ))}
      </RecordList>

      {openVendor === "" ? null : (
        <div data-os-vendor={openVendor}>
          <VendorPanel
            vendor={openVendor}
            reach={reach}
            door={doorFor(openVendor, registry.rows)}
            busy={actions.state.busy}
            onSave={(fields) => void actions.saveFederation(openVendor, fields)}
          />
        </div>
      )}

      <Panel label="What this node can call">
        <Subhead meta={!registry.loading && !registry.error && registry.fetchedAt !== null ? registry.rows.length : undefined}>What this node can call</Subhead>
        {registry.loading && registry.rows.length === 0 ? <RecordListSkeleton label="Reading the registry." /> : <Caption>{summarize(registry.rows).headline}</Caption>}
        {registry.rows.length === 0 ? null : (
          <RecordList as="ul" label="Registered providers">{registry.rows.map(p => <RecordRow key={p.name} name={p.name} secondary={`${vendorLabel(p.vendor)} ${p.model}, credential from ${sourceCopy(p.authSource)}`}
            state={p.available ? "can be called" : "cannot be called"} tone={p.available ? "accent" : "muted"}
            actions={<Button onClick={() => void actions.verify(p.name)} busy={actions.state.busy} busyLabel="Asking" ariaLabel={`Verify ${p.name} with the vendor`}>Verify</Button>}>
            {p.reason ? <span>{p.reason}</span> : null}
          </RecordRow>)}</RecordList>
        )}
        <div className="os-refresh-row">
          <Button onClick={registry.reload} busy={registry.loading}>
            Refresh
          </Button>
          <Caption>
            {registry.fetchedAt === null ? "" : `Read at ${new Date(registry.fetchedAt).toISOString()}. `}
            One node&apos;s own registry -- which replica answered is not
            knowable from here, which is why Apply broadcasts rather than
            relying on repeated reads.
          </Caption>
        </div>
      </Panel>
    </div>
  );
}

/**
 * One vendor: its name, what is true of it, that it bills, and at most one
 * act -- its own form, opened below. A local cluster's vendor has no form and
 * therefore no act.
 */
function VendorRow({ door, reach, open, onOpenVendor }: { door: DoorReading; reach: IssuerReach; open: boolean; onOpenVendor: () => void }) {
  const local = reach === "local";
  const word =
    door.state === "open" ? VENDOR_WORDS.open
      : door.state === "half" ? VENDOR_WORDS.half
        : door.state === "unknown" ? VENDOR_WORDS.unknown
          : local ? VENDOR_WORDS.closed : VENDOR_WORDS.unset;
  return (
    <div data-os-vendor-state={door.state} data-os-vendorid={door.id}>
      <RecordRow
        name={door.name}
        secondary={local ? "A local cluster's sign-in issuer is private, so neither vendor can verify it and no id typed here would ever be accepted. That is not a fault to fix." : door.said}
        state={word}
        tone={door.state === "open" ? "accent" : "muted"}
        actions={local ? null : <Button onClick={onOpenVendor} ariaExpanded={open}>{open ? "Close" : door.state === "open" ? `Edit ${door.name}` : `Set up ${door.name}`}</Button>}
      >
        <span>Billed per call.</span>{door.detail ? <span className="os-mono">{door.detail}</span> : null}
      </RecordRow>
    </div>
  );
}

/**
 * A vendor's own panel: its detail and its id form.
 *
 * Carried over from AI providers with its behaviour intact -- the same fields,
 * the same all-or-none save, the same absence on a local cluster. What changed
 * is that it is no longer standing on the page: it opens under the row that
 * names it.
 */
function VendorPanel({
  vendor,
  reach,
  door,
  busy,
  onSave,
}: {
  vendor: string;
  reach: IssuerReach;
  door: VendorDoor;
  busy: boolean;
  onSave: (fields: Record<string, string>) => void;
}) {
  const label = vendorLabel(vendor);
  return (
    <Panel label={label}>
      <div className="os-door-panel">
        <Subhead>{label}</Subhead>
        {door.said === "" ? null : <p className="os-door-detail os-mono">{door.said}</p>}
        {reach === "local" ? (
          <Caption>
            A local cluster&apos;s OIDC issuer is not reachable from the public
            internet, so {label} cannot verify a token it mints and no id you
            enter here could ever work. Uploading a JWKS per developer cluster
            is not reproducible, so this is not a gap waiting to be closed --
            use a machine you own as the source instead.
          </Caption>
        ) : (
          <FederationForm
            vendor={vendor}
            label={label}
            state={door.state}
            busy={busy}
            onSave={onSave}
          />
        )}
      </div>
    </Panel>
  );
}

/**
 * The federation form: ids from the vendor's own console, never a credential.
 *
 * ALL OR NONE. A partial set refuses boot -- hours after the save that caused
 * it -- so the client mirrors the engine's check and the Save control is
 * unavailable until every required field is filled. A blank optional field is
 * OMITTED rather than written empty, because an empty string is a value and
 * "not set" is not.
 *
 * WHAT AN UNTOUCHED FORM SAYS DEPENDS ON THE DOOR IT SITS UNDER, and all three
 * readings are load-bearing:
 *
 *  - OPEN. An empty form under "Open" reads as unfinished. Rotating onto a
 *    different service account is a real act and stays reachable; it is just
 *    not the act the panel is about, so the caption says so.
 *  - HALF. The engine's sentence above has just named some of these ids as
 *    SET. A form that then demanded only the missing one would build a set
 *    nobody had checked -- the write stores the set WHOLE -- so it asks for
 *    every id, and says why.
 *  - UNSET. Nothing has failed, so nothing is listed as missing. `touched` is
 *    what holds the "Still needed" line back until somebody is filling the
 *    form in, which is when it helps rather than reading as a complaint about
 *    the normal state of a new cluster.
 */
function FederationForm({
  vendor,
  label,
  state,
  busy,
  onSave,
}: {
  vendor: string;
  label: string;
  state: VendorDoor["state"];
  busy: boolean;
  onSave: (fields: Record<string, string>) => void;
}) {
  const fields = federationFields(vendor);
  const [draft, setDraft] = useState<Record<string, string>>({});
  const touched = Object.values(draft).some((v) => v.trim() !== "");
  const missing = missingFederationFields(vendor, draft);

  return (
    <form
      className="os-form"
      onSubmit={(e) => {
        e.preventDefault();
        const out: Record<string, string> = {};
        for (const f of fields) {
          const value = (draft[f.key] ?? "").trim();
          if (value !== "") out[f.key] = value;
        }
        onSave(out);
      }}
    >
      {fields.map((f) => (
        <Field key={f.key} label={f.required ? f.label : `${f.label} (optional)`}>
          <Input
            id={`provider-fed-${vendor}-${f.key}`}
            label={f.required ? f.label : `${f.label} (optional)`}
            value={draft[f.key] ?? ""}
            placeholder={f.hint}
            onChange={(next) => setDraft({ ...draft, [f.key]: next })}
          />
        </Field>
      ))}
      <Caption>
        {!touched
          ? state === "open"
            ? `${label}'s ids are already in use. Filling these in replaces them at the next Apply -- nothing changes until then.`
            : state === "half"
              ? `Some of ${label}'s ids are set and some are not, and a node that reads a partial set refuses to boot. Fix it before anything restarts -- and re-enter every id below, not only the missing one: the write stores the set whole.`
              : `Ids from ${label}'s own console, not credentials -- they are stored as plaintext rows. The projected token path is not asked for here: it comes from the deployment, beside the volume it names.`
          : missing.length === 0
            ? "Saving stores the ids. Apply is what makes every node read them."
            : `Still needed: ${missing.map((f) => f.label).join(", ")}. A partial set refuses boot, so it cannot be saved.`}
      </Caption>
      <Button
        type="submit"
        tone="primary"
        busy={busy}
        busyLabel="Saving"
        disabled={missing.length > 0}
      >
        Save {label} ids
      </Button>
    </form>
  );
}
