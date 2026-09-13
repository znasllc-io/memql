import { useEffect, useState } from "react";

import { Button, Caption, Field, Head, Input, Notice, Panel, Subhead } from "../../kit";
import { findRegion, revealRegion } from "../../kit";
import { useAppReach } from "../../kit/ReadinessStates";
import { useSession } from "../../chrome/access";
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
import {
  DOOR_COST,
  doorReadings,
  useInferenceStatus,
  type DoorId,
  type DoorReading,
} from "./routingFacts";

// Settings -> Doors (epic memql#5153, D1). Where inference can come from, and
// in what order a call tries.
//
// ===========================================================================
// WHAT THIS SCREEN REPLACES, AND WHY IT IS A LIST
// ===========================================================================
// "AI providers" was three stacked panels, each carrying its state, its
// explanation and its form -- so the page was mostly prose and the reader had
// to hold three panels in their head to answer the one question they came
// with: where does a call go? The record's word for the problem is "words all
// over".
//
// A door is a PLACE, four places is a list, and a list answers that question
// by being read top to bottom. The forms did not get smaller; they moved off
// the standing page and behind the row they belong to, which is DESIGN.md
// rule 9 -- real estate belongs to content -- applied to a screen whose
// content is four lines.
//
// ===========================================================================
// THE ORDER IS THE PRODUCT, AND IT IS ON THE SCREEN
// ===========================================================================
// The previous screen ordered its panels by what was most likely to be
// USEFUL: federation first on a cluster that could federate. That was a
// reasonable answer to a different question, and it is the wrong one here,
// because it put the metered door at the top of a product whose whole thesis
// is that the metered door is last.
//
// The order here is the TRY ORDER, and it does not vary: a machine you own,
// then a subscription you already pay for, then a vendor that bills. An owner
// who wants a vendor first says so in a rule, and every decision record then
// reports that they did. Nothing on this page reorders itself to flatter a
// cluster's current state -- a fresh cluster reads the same list as a
// configured one, and the two free doors being shut is the thing it should be
// showing.
//
// ===========================================================================
// THE FIVE RULES CARRIED OVER FROM "AI PROVIDERS", UNCHANGED
// ===========================================================================
// They were right and this is a re-shaping, not a re-litigation:
//
//  1. THE STATE IS A COLUMN, AND IT IS THE CONTENT. Every door's state word
//     sits in one column so the whole cluster is read by scanning it. Not a
//     pill in a card corner: a pill puts the answer in the furniture, and its
//     palette has no way to say "nothing here, and that is correct".
//  2. AN UNSET DOOR IS THE QUIETEST THING ON THE SCREEN. A fresh cluster has
//     no federated vendor and every local cluster has none permanently. An
//     operator who meets a warning banner on a fresh install concludes the
//     install failed.
//  3. HALF-CONFIGURED IS THE ONE THAT SHOUTS, because the engine REFUSES BOOT
//     on it -- hours after the save that caused it. It carries the warn rule
//     and renders the engine's own sentence, which names which ids are set
//     better than a re-derivation here would.
//  4. A LOCAL CLUSTER IS TOLD, NOT ASKED. Its OIDC issuer is private, so
//     neither vendor can discover it and no id anybody types will ever work.
//     The form is ABSENT with a sentence in its place -- rule 12's position,
//     an act that is not legal is absent rather than disabled.
//  5. VERIFY REACHES A VENDOR, SO A PERSON PRESSES IT. A live credential check
//     spends somebody's quota with a third party. Never on render, and a
//     refusal is a RESULT in the vendor's own words.
//
// AND A SAVE IS STILL NOT AN APPLY. Saving writes the ids; the registry each
// node resolved at boot does not move until Apply broadcasts. Two controls,
// because they are two facts.

/**
 * The section's role requirement (D7).
 *
 * OWNER OR DEVELOPER, AS A SET, EXPLICITLY NOT ADMIN -- carried over from AI
 * providers unchanged, along with its argument. A developer helps an owner
 * through setup, so the provider builtins are owner-or-developer; an admin's
 * concern is USER ADMINISTRATION, and the ladder puts admin (200) below
 * developer (300), so `{ min: "developer" }` would admit exactly the role the
 * engine refuses and offer them a form that fails field by field.
 *
 * Presentation only; every gate is server-side. The manifest declares it and
 * this constant is the one copy of the value.
 */
export const DOORS_SECTION_RESOURCE = "app:settings/providers";

/**
 * The state words, in one place because they are the section's whole
 * vocabulary and the suite reads them from here rather than restating them.
 *
 * `closed` and `unset` are the SAME door state wearing two words, and the
 * distinction is worth the second word: "Not set up" invites somebody to set
 * it up, which on a local cluster is an invitation to waste an afternoon.
 */
export const DOOR_WORDS = {
  open: "Open",
  unset: "Not set up",
  closed: "Closed",
  half: "Half set up",
  unknown: "Not read",
} as const;

const VENDOR_IDS: readonly DoorId[] = ["anthropic", "openai"];

export function DoorsSection({
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
  const fleetApp = useAppReach("fleet");

  // WHICH VENDOR FORM IS OPEN. Empty means none, which is the standing state:
  // the page is four lines until somebody asks for a form.
  const [openVendor, setOpenVendor] = useState<string>("");

  const doors = doorReadings(inference.status, (vendor) => doorFor(vendor, registry.rows));

  // ARRIVING AT A VENDOR (epic memql#5106, carried over). The first-run wizard
  // sends somebody here to set a named vendor up, and landing at the top of
  // the page would leave the last step of that act to them.
  //
  // TWO EFFECTS, NOT ONE, and the split is load-bearing. The panel is now
  // opened on demand, so on the tick the intent arrives its region is not in
  // the DOM yet and `findRegion` would answer null -- a silent no-op that
  // reads exactly like a broken intent. The first effect opens it; the second
  // runs once the region exists and is what consumes the intent, so the
  // instruction is not eaten by a render that could not act on it.
  const vendor = typeof intent?.payload["vendor"] === "string" ? intent.payload["vendor"] : "";
  const wanted = VENDOR_IDS.includes(vendor as DoorId) ? vendor : "";
  useEffect(() => {
    if (!intent || vendor === "") return;
    if (wanted === "") {
      // A vendor this page does not serve. Reveal nothing, but CONSUME it --
      // an instruction nobody can act on must not be delivered again forever.
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

  const openCount = doors.filter((d) => d.state === "open").length;

  return (
    <div className="os-settings">
      <Head title="Doors" meta={`${openCount} of ${doors.length} open`}>
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
        A door is a place a model can come from. A call tries them in the order
        below and takes the first one that is open, so the two that cost nothing
        are tried before the two that bill. Federation is the only door for a
        cloud vendor -- there is no API key to enter, here or anywhere else in
        the product.
      </p>

      {registry.error ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.role || "your role"}.`}
          detail={registry.error}
        />
      ) : null}

      {actions.state.message ? (
        <Notice
          tone={actions.state.failed ? "error" : "info"}
          sentence={actions.state.failed ? "That did not go through." : "Done."}
          detail={actions.state.message}
        />
      ) : null}

      <ol className="os-doorlist" aria-label="Doors, in the order a call tries them">
        {doors.map((door) => (
          <DoorRow
            key={door.id}
            door={door}
            reach={reach}
            open={openVendor === door.id}
            onOpenVendor={() => setOpenVendor(openVendor === door.id ? "" : door.id)}
            onAddMachine={
              fleetApp.sections.includes("machines") && fleetApp.canOpenWindows
                ? () => fleetApp.open("machines", { addMachine: { inference: true } })
                : null
            }
            onOpenApps={
              fleetApp.sections.includes("apps") && fleetApp.canOpenWindows
                ? () => fleetApp.open("apps")
                : null
            }
          />
        ))}
      </ol>

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
        <Subhead>What this node can call</Subhead>
        <Caption>
          {registry.loading && registry.rows.length === 0
            ? "Reading the registry."
            : summarize(registry.rows).headline}
        </Caption>
        {registry.rows.length === 0 ? null : (
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

/**
 * One door in the list: its name, what is true of it, what it costs, and at
 * most one act.
 *
 * THE COST LINE IS NOT DECORATION. It is the only place a person learns, at
 * the moment they are deciding, that the first two doors spend nothing and the
 * last two bill -- which is the fact the whole feature exists to act on. It is
 * stated per door rather than once at the top because a reader scanning for
 * "the shut one" reads that row and nothing else.
 *
 * AT MOST ONE ACT, per D1 and DESIGN.md rule 1's spirit. The fleet row opens
 * Add machine with the inference intent, the apps row opens Fleet -> Apps, a
 * vendor row opens its own form below. An act that cannot be reached -- no
 * window to open into, or a role that cannot see Fleet -- is ABSENT and the
 * row says where to go in words instead, which is rule 12.
 */
function DoorRow({
  door,
  reach,
  open,
  onOpenVendor,
  onAddMachine,
  onOpenApps,
}: {
  door: DoorReading;
  reach: IssuerReach;
  open: boolean;
  onOpenVendor: () => void;
  onAddMachine: (() => void) | null;
  onOpenApps: (() => void) | null;
}) {
  const word =
    door.state === "open"
      ? DOOR_WORDS.open
      : door.state === "half"
        ? DOOR_WORDS.half
        : door.state === "unknown"
          ? DOOR_WORDS.unknown
          : // A local cluster cannot federate at all, so its vendor doors are
            // CLOSED rather than "Not set up": the second word invites an
            // afternoon of work that cannot succeed.
            door.kind === "federation" && reach === "local"
            ? DOOR_WORDS.closed
            : DOOR_WORDS.unset;

  return (
    <li className="os-doorrow" data-os-door={door.state} data-os-doorid={door.id}>
      <div className="os-doorrow-main">
        <p className="os-doorrow-name">{door.name}</p>
        <p className="os-doorrow-said">
          {door.kind === "federation" && reach === "local"
            ? "A local cluster's OIDC issuer is private, so neither vendor can discover it and no id typed here would ever be accepted. Nothing here will work, and that is not a fault to fix."
            : door.said}
        </p>
        <p className="os-doorrow-cost">{DOOR_COST[door.id]}</p>
        {door.detail === "" ? null : <p className="os-door-detail os-mono">{door.detail}</p>}
      </div>
      <div className="os-doorrow-state">
        <p className="os-door-state">{word}</p>
        <DoorAct
          door={door}
          reach={reach}
          open={open}
          onOpenVendor={onOpenVendor}
          onAddMachine={onAddMachine}
          onOpenApps={onOpenApps}
        />
      </div>
    </li>
  );
}

function DoorAct({
  door,
  reach,
  open,
  onOpenVendor,
  onAddMachine,
  onOpenApps,
}: {
  door: DoorReading;
  reach: IssuerReach;
  open: boolean;
  onOpenVendor: () => void;
  onAddMachine: (() => void) | null;
  onOpenApps: (() => void) | null;
}) {
  if (door.id === "fleet") {
    if (onAddMachine === null) {
      return <Caption>Pair a machine in Fleet, under Machines.</Caption>;
    }
    return <Button onClick={onAddMachine}>Add a machine</Button>;
  }
  if (door.id === "app") {
    if (onOpenApps === null) {
      return <Caption>Sign in to Claude Code or Codex on a machine, then set delegation in Fleet, under Apps.</Caption>;
    }
    return <Button onClick={onOpenApps}>Open Apps</Button>;
  }
  // A local cluster's vendor form is absent, so the act that opens it is too.
  if (reach === "local") return null;
  return (
    <Button onClick={onOpenVendor} ariaExpanded={open}>
      {open ? "Close" : door.state === "open" ? `Edit ${door.name}` : `Set up ${door.name}`}
    </Button>
  );
}

/**
 * A vendor's own panel: the door's detail and the federation form.
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
            reach a model through a machine you own instead.
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
        ariaLabel={`Save ${label} federation`}
      >
        Save {label} federation
      </Button>
    </form>
  );
}
