import { useState } from "react";

import { Button, Caption, Chip, Chips, Fact, Facts, Field, Input, Notice } from "../../kit";
import { useSession } from "../../chrome/access";
import type { RoleRequirement } from "../../system/roles";
import {
  configurableCards,
  lanesOf,
  silentCards,
  visibleReasons,
  type ConfigureOutcome,
  type IntegrationCard,
  type IntegrationReason,
  type IntegrationSlot,
} from "./integrationsReport";
import {
  integrationBlurb,
  integrationLabel,
  laneLabel,
  slotLabel,
  sourceLabel,
  stateLabel,
} from "./integrationCopy";
import { CONFIG_WRITE_NOTE } from "./integrationWrites";
import { useIntegrations, type IntegrationsFacts } from "./useIntegrations";

// Integrations (issue #4826 / program decision P6): what this cluster can
// talk to, whether it is set up, and what each key is for.
//
// OWNER OR DEVELOPER, EXPLICITLY NOT ADMIN. Wiring up what the cluster talks
// to is a developer's concern; administering PEOPLE is an admin's. The
// requirement is a role SET rather than a floor because the ladder puts admin
// between the two members and cannot leave it out -- see system/roles.ts. The
// manifest is where the section declares it; this constant is the one copy of
// the value.
//
// THE ENGINE ADMITS ONE ROLE MORE, AND THAT IS NOT A DISAGREEMENT.
// `integrations/email/status.go`'s `statusAuthorized` admits owner, developer
// and admin (memql#4826 added developer -- its absence refused the read to the
// role this section exists for). Admin is kept there on purpose: P6 is about
// who may CONFIGURE, this read carries no secret, and narrowing it would take
// a capability from every admin in every deployment to make a sentence
// symmetrical. Declining to OFFER an admin the section is this manifest's job,
// and that is where the distinction belongs -- the gate here is presentation
// and the engine's is the authority, which is the shell's standing rule.
//
// A refusal still renders in surface in the engine's own words, because
// somebody below the floor who saw an empty section would read it as "nothing
// is configured" -- and A REFUSAL IS NOT A ZERO.
//
// THREE RULES, and they are the design of this surface:
//
//  1. A SECRET IS WRITE-ONLY, and that is now literal in both directions: the
//     field POSTS a value and the card never renders one back -- not masked,
//     not truncated, not dots. The plaintext crosses the wire once, is sealed
//     server-side under a key that must never exist in a browser, and no reply
//     carries it again. `TestStatusNeverLeaksACredential` holds the server
//     half; the client half is `IntegrationSlot` having nowhere for a secret's
//     value to land and no control that would display one. The rotate command
//     stays beside the field for operators who would rather use the CLI.
//  2. A BOOT-ENVELOPE VARIABLE IS LISTED, NOT OFFERED. The resolver reads env
//     first and stops, so a value the environment supplies cannot be
//     overridden from the graph. That boundary is DECLARED
//     (component/envregistry/manifest.yaml) and arrives as the slot's own
//     `editable` flag; it is never re-derived here. An editable-looking field
//     that cannot work is worse than an absent one.
//  3. UNCONFIGURED IS A NORMAL STATE. A fresh cluster boots clean, so that
//     card is an invitation -- what this would let you do, and what it needs.
//     The error voice is reserved for `unhealthy`, which means configured and
//     failing, and there the engine's reason is the whole message.

/** The section's role floor. Presentation only; every gate is server-side. */
export const INTEGRATIONS_SECTION_ROLE: RoleRequirement = { any: ["owner", "developer"] };

export function IntegrationsSection() {
  const { access } = useSession();
  const facts = useIntegrations();
  const cards = configurableCards(facts.report);
  const silent = silentCards(facts.report);

  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Integrations</h3>
      <p className="os-caption">
        What this cluster can talk to, and what each one needs before it will.
        Read from the node that answered -- integrations are process state, so
        two replicas can genuinely disagree, and which one answered is not
        knowable from here.
      </p>

      {facts.error ? (
        <Refusal role={access?.clusterRole ?? ""} message={facts.error} />
      ) : facts.report === null ? (
        <Caption>
          {facts.loading ? "Loading from the cluster" : "No integration report came back."}
        </Caption>
      ) : (
        <>
          {cards.length === 0 ? (
            <Caption>
              This node registered no integration that publishes a
              configuration report. That is an answer about this build, not a
              cluster that is missing something.
            </Caption>
          ) : (
            cards.map((card) => (
              <IntegrationPanel key={card.name} card={card} facts={facts} />
            ))
          )}
          <SilentRollCall cards={silent} />
        </>
      )}

      <Refresh facts={facts} />
    </div>
  );
}

function IntegrationPanel({
  card,
  facts,
}: {
  card: IntegrationCard;
  facts: IntegrationsFacts;
}) {
  const label = integrationLabel(card.name);
  const blurb = integrationBlurb(card.name);
  return (
    <section className="os-field-group" aria-label={label}>
      <h4 className="os-subhead">{label}</h4>
      <Chips label={`${label} state`}>
        <Chip tone={card.state === "configured" ? "accent" : "neutral"}>
          {stateLabel(card.state)}
        </Chip>
        {card.mode ? <Chip tone="muted">{card.mode}</Chip> : null}
      </Chips>

      {/* The engine's own sentences, verbatim and whole. They name the lane
          rule, the log-only trap and the probe verdict, and every paraphrase
          this window could write would drop one of them. */}
      {card.state === "unhealthy" ? (
        <Notice
          tone="error"
          sentence={`${label} is configured and the provider is refusing it.`}
          detail={card.detail}
        />
      ) : card.state === "needs_configuration" ? (
        <Notice
          tone="info"
          sentence={blurb || undefined}
          next="Fill the keys below and it starts working. Nothing is broken until then."
          detail={card.detail}
        />
      ) : card.state === "configured" ? (
        <Notice tone="info" sentence={`${label} is set up.`} detail={card.detail} />
      ) : (
        <Notice
          tone="info"
          sentence={`${label} publishes no configuration report.`}
          detail={card.detail}
        />
      )}

      {/* The engine's own two words, beside the state chip that summarises
          them. Not a duplication: the tri-state's "unknown is not no" is a
          real distinction -- an integration nobody has asked about and one
          that answered no are different situations -- and "Provider check:
          unknown" is how a reader learns a live check has never been run. The
          labels say what each answers rather than repeating the chip's word,
          which is also what stops "Configured" naming two things on one
          card. */}
      <Reasons reasons={visibleReasons(card)} />

      <Facts>
        <Fact label="Set up" value={card.configured} />
        <Fact label="Provider check" value={card.health} />
        <Fact
          label="Capabilities"
          value={card.capabilities.length === 0 ? "" : card.capabilities.join(", ")}
          mono
        />
      </Facts>

      <LiveCheck card={card} facts={facts} />

      {card.slots.length === 0 ? (
        <Caption>This integration reports no configuration keys.</Caption>
      ) : (
        <>
          {lanesOf(card).map((lane) => (
            <div key={lane.name} role="group" aria-label={`${label} -- ${laneLabel(lane.name)}`}>
              <h5 className="os-subhead">{laneLabel(lane.name)}</h5>
              {/* Each key is its own bordered box (`os-field-group`, the shape
                  every other Settings group already uses) rather than a line
                  in a flat list: a slot is four blocks -- state, field,
                  purpose, and for a credential the rotate command -- and four
                  blocks at a four-pixel gap run into the next slot's four. No
                  new CSS: this section is built from the vocabulary that
                  exists. */}
              <ul
                className="os-hidden-list"
                aria-label={`${label} ${laneLabel(lane.name)} configuration`}
              >
                {lane.slots.map((slot) => (
                  <li
                    className="os-field-group"
                    key={`${slot.secret ? "secret" : "setting"}:${slot.name}`}
                  >
                    <SlotRow slot={slot} facts={facts} />
                  </li>
                ))}
              </ul>
            </div>
          ))}
          {lanesOf(card).length > 1 ? (
            <Caption>
              These are alternatives, and a lane is taken WHOLE or not at all --
              from this node&apos;s environment, or from settings stored in the
              cluster, never half of each. Filling in some of both is the one
              arrangement that resolves to nothing.
            </Caption>
          ) : null}
          <Caption>{CONFIG_WRITE_NOTE}</Caption>
        </>
      )}
    </section>
  );
}

/**
 * One configuration key: what it is, whether it is set, where from, and the
 * field that changes it.
 *
 * THE DRAFT IS `null` UNTIL SOMEBODY TYPES, and that is what keeps a save from
 * fighting the read that follows it. `null` means "show the server's value",
 * so a successful save resets to `null` and the reloaded row is what appears
 * -- no effect syncing state, and no box holding stale text next to a card
 * that has moved on. For a credential the server value is the empty string by
 * construction, so the field clears itself.
 *
 * THE SECRET FIELD IS NOT MASKED, deliberately. It is typed or pasted once and
 * never read back, so masking protects nothing that the card does not already
 * refuse to show -- while hiding a trailing character in a pasted credential,
 * which then fails at send time as an opaque vendor error. It is the posture
 * every write-only secret field in this kind of console takes, and the caption
 * says the value is sent once.
 */
function SlotRow({ slot, facts }: { slot: IntegrationSlot; facts: IntegrationsFacts }) {
  const label = slotLabel(slot.name);
  const [draft, setDraft] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [outcome, setOutcome] = useState<ConfigureOutcome | null>(null);
  const [returned, setReturned] = useState(false);

  // The engine already drew the sensitivity line, and this honours it rather
  // than drawing a second one: a `Setting` is the non-secret half and its
  // value is meant to be read (which mailbox does this cluster send as), a
  // `Credential` carries no value at all. Withholding an env-sourced setting
  // would hide the sending mailbox on every production cluster, where env is
  // the normal source -- env decides EDITABILITY here, not sensitivity.
  const offersAField = slot.editable;
  const value = draft ?? (slot.secret ? "" : slot.value);
  const ready = value.trim() !== "";

  const save = () => {
    if (!ready || saving) return;
    setSaving(true);
    setError("");
    setOutcome(null);
    setReturned(false);
    facts
      .configure(slot.name, value.trim())
      .then((result) => {
        setOutcome(result);
        setReturned(true);
        // Drop the draft BEFORE the re-read, so the field shows what the
        // cluster now holds rather than what was typed at it. For a credential
        // that is the empty string, which is the field clearing itself.
        setDraft(null);
        facts.reload();
      })
      .catch((err: unknown) => setError(messageOf(err)))
      .finally(() => setSaving(false));
  };

  return (
    <>
      <Chips label={`${label} state`}>
        <Chip tone={slot.present ? "accent" : "muted"}>{slot.present ? "Set" : "Not set"}</Chip>
        {/* A slot that is not set has no source, and a second chip reading
            "No source" beside "Not set" says the same thing twice. */}
        {slot.present ? (
          <Chip tone="neutral" title={slot.envVar || undefined}>
            {sourceLabel(slot.source)}
          </Chip>
        ) : null}
        {slot.secret ? <Chip tone="muted">Write-only</Chip> : null}
        {/* Only OPTIONAL is worth a chip. Marking eleven slots "Required"
            marks nothing; marking the two that are not is what tells somebody
            which fields they can leave alone. */}
        {slot.required ? null : <Chip tone="muted">Optional</Chip>}
      </Chips>
      <Field label={`${label}${slot.envVar ? ` -- ${slot.envVar}` : ""}`}>
        {offersAField ? (
          <>
            <Input
              id={`integration-slot-${slot.name}`}
              label={slot.secret ? `${label} (new value)` : label}
              value={value}
              onChange={setDraft}
              placeholder={slot.secret && slot.present ? "Replace this credential" : undefined}
              onEnter={save}
            />
            <Button
              tone="primary"
              onClick={save}
              busy={saving}
              busyLabel="Saving"
              disabled={!ready}
              ariaLabel={`Save ${label}`}
            >
              Save
            </Button>
          </>
        ) : (
          // LISTED, NOT OFFERED. The resolver reads env first and stops, so a
          // field here would accept a value and change nothing.
          <>
            <span className="os-mono">{slot.secret ? "" : slot.value || "--"}</span>
            <Caption>
              Set in this node&apos;s environment, which the resolver reads
              first. Changing it is a redeploy.
            </Caption>
          </>
        )}
      </Field>
      {slot.purpose ? <p className="os-caption">{slot.purpose}</p> : null}
      {slot.secret && offersAField ? (
        <Caption>
          Sent once and sealed in the cluster. Nothing shows it again -- if you
          need to change it, type the new value here.
        </Caption>
      ) : null}
      {/* The engine writes TWO sentences about a slot that is wrong -- a short
          one for this position and a longer one for the card's summary -- and
          both are rendered where their author put them. `reason` is empty
          whenever nothing is wrong, which is what keeps a healthy list quiet. */}
      {slot.reason ? <Notice tone="warn" detail={slot.reason} /> : null}
      {/* A refusal beside the control that produced it, in the engine's words:
          it names the slots that exist when a name is wrong, and says why an
          empty value is refused. Never a toast, never rewritten. */}
      {error ? <Notice tone="error" sentence={`${label} was not saved.`} detail={error} /> : null}
      {/* THE SUCCESS LINE IS THE ENGINE'S. `takesEffect` has two forms -- this
          node re-resolves on its next send, or this node runs from an
          environment that outranks stored rows and will keep doing so -- and
          only the engine knows which happened. A client-authored "Saved, it
          takes effect shortly" would be confidently wrong half the time. When
          the reply carries no sentence at all we say the write returned and
          claim nothing about when it lands. */}
      {returned ? (
        <Notice
          tone="info"
          detail={outcome?.takesEffect || undefined}
          sentence={
            outcome === null
              ? `${label} was written. The cluster did not say when it takes effect.`
              : undefined
          }
        />
      ) : null}
      {slot.secret && slot.rotate ? (
        <p className="os-caption os-mono">{slot.rotate}</p>
      ) : null}
    </>
  );
}

/**
 * Why the state is not `configured`, one entry per thing somebody would do.
 *
 * VERBATIM AND IN THE ENGINE'S ORDER. The reasons are built per lane and each
 * names its own env var, so this is the scannable answer to "what do I have to
 * set" -- and the `code` is used to decide EMPHASIS and nothing else. A
 * surface that branched on a code to compose its own sentence would be writing
 * the message it was checking.
 *
 * A reason with no `slot` belongs to no field -- a split lane, a failed probe,
 * a refused mode -- and that is exactly the kind the slot list below cannot
 * carry, which is why this list is not a duplicate of it.
 */
function Reasons({ reasons }: { reasons: readonly IntegrationReason[] }) {
  if (reasons.length === 0) return null;
  return (
    <>
      <h5 className="os-subhead">What has to happen</h5>
      <ul className="os-hidden-list" aria-label="What has to happen">
        {reasons.map((reason, index) => (
          <li key={`${reason.code}:${reason.lane}:${reason.slot}:${index}`}>
            <Chips label={`Reason ${index + 1}`}>
              {reason.lane ? <Chip tone="neutral">{laneLabel(reason.lane)}</Chip> : null}
              {reason.slot ? <Chip tone="muted">{slotLabel(reason.slot)}</Chip> : null}
            </Chips>
            <p className="os-caption">{reason.detail}</p>
          </li>
        ))}
      </ul>
    </>
  );
}

/**
 * The live check.
 *
 * An ACTION, never something a render does: it dials the vendor. It sends no
 * mail either -- Graph acquires a client-credentials token, SMTP stops before
 * MAIL FROM -- and the button says so, because "check connection" next to a
 * mail integration reads like "send a test message" otherwise.
 */
function LiveCheck({ card, facts }: { card: IntegrationCard; facts: IntegrationsFacts }) {
  return (
    <div className="os-refresh-row">
      <Button onClick={facts.check} busy={facts.checking} busyLabel="Checking">
        Check connection
      </Button>
      <Caption>
        {card.probed
          ? `Checked at ${facts.report?.checkedAt || "an unrecorded time"} -- the verdict is in the account above. `
          : "Not checked yet; what is above is the configuration only. "}
        A check proves the credentials are accepted and sends nothing: a token
        request for Microsoft Graph, or connect / EHLO / STARTTLS /
        authenticate / quit for SMTP. Nobody is mailed.
      </Caption>
    </div>
  );
}

/**
 * Everything else the node registered.
 *
 * A LINE EACH, NOT A CARD EACH. A card promises there is something to
 * configure; these publish no self-report, so whether their credentials
 * resolved is not knowable from here. Saying that once for the group is the
 * accurate answer -- and inventing a card per name would be this window
 * making up a fact about the node.
 */
function SilentRollCall({ cards }: { cards: readonly IntegrationCard[] }) {
  if (cards.length === 0) return null;
  return (
    <section className="os-field-group" aria-label="Also registered on this node">
      <h4 className="os-subhead">Also registered on this node</h4>
      <ul className="os-hidden-list" aria-label="Integrations with no configuration report">
        {cards.map((card) => (
          <li key={card.name}>
            <span className="os-mono">{card.name}</span>
          </li>
        ))}
      </ul>
      <Caption>
        These publish no configuration report, so whether their credentials
        resolved is not knowable from here. That is the accurate answer, not a
        gap -- and it is per NODE: which plug-ins compile in is a build-tag
        decision, so a bff and an agent replica legitimately list different
        sets.
      </Caption>
    </section>
  );
}

/**
 * A refusal renders WHERE THE PANEL WOULD BE, in the engine's own words.
 *
 * Never rewritten: the engine's sentence names the roles its own check admits,
 * and this window's role gate is presentation over it rather than a second
 * authority. Saying so is what stops a refusal reading as a defect in either
 * half.
 */
function Refusal({ role, message }: { role: string; message: string }) {
  return (
    <Notice
      tone="warn"
      sentence={`The cluster declined this read for ${role || "your role"}.`}
      next="Nothing here is inferred from a refusal -- this is not an empty configuration, it is a read that did not happen. Which roles this window OFFERS the section to is presentation; which roles the cluster serves is the cluster's own decision, and it is the one that counts."
      detail={message}
    />
  );
}

/**
 * `checkedAt` wins when the reply carries one: `integrationStatus` stamps it
 * in the handler, and the moment the NODE looked is the honest answer. The
 * client clock only says when the reply arrived here.
 */
function Refresh({ facts }: { facts: IntegrationsFacts }) {
  const stamp =
    facts.report?.checkedAt ||
    (facts.fetchedAt === null ? "" : new Date(facts.fetchedAt).toISOString());
  return (
    <div className="os-refresh-row">
      <Button onClick={facts.reload} busy={facts.loading} busyLabel="Reading">
        Refresh
      </Button>
      <Caption>
        {stamp ? `Read at ${stamp}. ` : ""}A projection of one node&apos;s own
        registry -- which replica answered is not knowable from here.
      </Caption>
    </div>
  );
}

function messageOf(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
