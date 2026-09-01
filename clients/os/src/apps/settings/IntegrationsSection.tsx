import { Button, Caption, Chip, Chips, Fact, Facts, Field, Input, Notice } from "../../kit";
import { useSession } from "../../chrome/access";
import type { RoleRequirement } from "../../system/roles";
import {
  configurableCards,
  silentCards,
  type IntegrationCard,
  type IntegrationSlot,
} from "./integrationsReport";
import {
  integrationBlurb,
  integrationLabel,
  slotLabel,
  sourceLabel,
  stateLabel,
} from "./integrationCopy";
import { INTEGRATION_WRITES } from "./integrationWrites";
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
// AND THE ENGINE DOES NOT AGREE YET. `integrations/email/status.go`'s
// `statusAuthorized` admits owner and ADMIN -- so a developer, the role this
// section exists for, is refused the read today and an admin who reached the
// section by deep link would be served. Neither is papered over: the refusal
// renders in surface in the engine's own words with a line saying which gate
// disagreed, because a developer seeing an empty section would read it as
// "nothing is configured" -- and A REFUSAL IS NOT A ZERO.
//
// THREE RULES, and they are the design of this surface:
//
//  1. A SECRET IS WRITE-ONLY. No value is read back -- not masked, not
//     truncated, not dots. The card says whether it is SET and where it came
//     from. `TestStatusNeverLeaksACredential` holds the server half; the
//     client half is offering no control that would display one, which is why
//     `IntegrationSlot` has nowhere for a secret's value to land.
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
          {/* Each key is its own bordered box (`os-field-group`, the shape
              every other Settings group already uses) rather than a line in a
              flat list: a slot is four blocks -- state, field, purpose, and
              for a credential the rotate command -- and four blocks at a
              four-pixel gap run into the next slot's four. No new CSS: this
              section is built from the vocabulary that exists. */}
          <ul className="os-hidden-list" aria-label={`${label} configuration`}>
            {card.slots.map((slot) => (
              <li
                className="os-field-group"
                key={`${slot.secret ? "secret" : "setting"}:${slot.name}`}
              >
                <SlotRow slot={slot} />
              </li>
            ))}
          </ul>
          {INTEGRATION_WRITES.available ? null : (
            <Notice
              tone="warn"
              sentence="Saving from here is not wired up yet."
              next={INTEGRATION_WRITES.reason}
            />
          )}
        </>
      )}
    </section>
  );
}

/**
 * One configuration key.
 *
 * THE FIELD IS DISABLED RATHER THAN ABSENT, and for a secret it is disabled
 * rather than typeable. The shape of what will exist is worth showing -- it
 * is what says a value belongs here at all -- but a box that accepts a client
 * secret and then cannot save it is a box somebody pastes a credential into
 * and loses. "Nothing happens where nothing is offered" is the shell's rule;
 * a disabled control offers nothing while still saying what it is for.
 */
function SlotRow({ slot }: { slot: IntegrationSlot }) {
  const label = slotLabel(slot.name);
  // The engine already drew the sensitivity line, and this honours it rather
  // than drawing a second one: a `Setting` is the non-secret half and its
  // value is meant to be read (which mailbox does this cluster send as), a
  // `Credential` carries no value at all. Withholding an env-sourced setting
  // would hide the sending mailbox on every production cluster, where env is
  // the normal source -- env decides EDITABILITY here, not sensitivity.
  const offersAField = !slot.secret && slot.editable;
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
      </Chips>
      <Field label={`${label}${slot.envVar ? ` -- ${slot.envVar}` : ""}`}>
        {slot.secret ? (
          // NO INPUT AND NO VALUE. The reply carries neither, and a field here
          // would be the one place in this shell a credential could be shown.
          <Caption>
            Changed with the operator command below, never from a browser.
          </Caption>
        ) : offersAField ? (
          <Input
            id={`integration-slot-${slot.name}`}
            label={label}
            value={slot.value}
            onChange={() => {}}
            disabled
          />
        ) : (
          // LISTED, NOT OFFERED. The resolver reads env first and stops, so a
          // field here would accept a value and change nothing.
          <>
            <span className="os-mono">{slot.value || "--"}</span>
            <Caption>
              Set in this node&apos;s environment, which the resolver reads
              first. Changing it is a redeploy.
            </Caption>
          </>
        )}
      </Field>
      {slot.purpose ? <p className="os-caption">{slot.purpose}</p> : null}
      {slot.secret && slot.rotate ? (
        <p className="os-caption os-mono">{slot.rotate}</p>
      ) : null}
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
 * The gap is named rather than smoothed over. This section is gated
 * owner-or-developer; `integration.email.status` admits owner and ADMIN. A
 * developer refused here has hit that disagreement, and telling them so is
 * the difference between a bug report and a shrug.
 */
function Refusal({ role, message }: { role: string; message: string }) {
  return (
    <Notice
      tone="warn"
      sentence={`The cluster declined this read for ${role || "your role"}.`}
      next={
        role === "developer"
          ? "This section is offered to owners and developers; the engine's own check on this read admits owners and admins. Closing that gap is engine work, and until it lands a developer cannot read it."
          : "Nothing is inferred from a refusal -- this is not an empty configuration, it is a read that did not happen."
      }
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
