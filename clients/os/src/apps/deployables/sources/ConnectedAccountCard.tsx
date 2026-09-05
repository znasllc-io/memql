import { useState, type ReactNode } from "react";

import { Button, Caption, Chip, Chips, Fact, Facts } from "../../../kit";
import type { Refusal } from "../packages/actions";
import { toneFor } from "../packages/refusals";
import { ProblemNotice } from "../packages/ReportView";
import { InstallLink } from "./RepositoryPicker";
import type { InstallationRow, PendingInstallation } from "./repositories";
import { credentialIsRevoked, type CredentialRow } from "./rows";

// The connected-account card: which GitHub account this cluster acts as, and
// the one act that ends it (epic memql#4915, design section A step 6).
//
// ===========================================================================
// THE CARD'S PRESENCE IS THE STATE
// ===========================================================================
// There is no green "Connected" pill, because a card that exists says
// exactly that. What a person actually wants to know is WHICH account, so
// the accent chip carries the login and the rest is facts. No new card
// language either: `section.os-field-group` with an `h4.os-subhead` is the
// Integrations pattern, and this is a settings group like any other.
//
// SAY IT ONCE (DESIGN.md rule 7). The installation chips ARE the "reaches"
// fact, so there is no `Reaches` row repeating them in the Facts block --
// the first draft had both, which read as thorough and was one thing
// captioned twice.
//
// PROP-DRIVEN, so the group that mounts it owns the round trips and the
// refusals. `installations` and `pending` carry LOGINS, which only
// `sourceRepositories` answers -- the credential row itself projects
// installation IDS, and an id is not a fact anybody can read. When they are
// not supplied the card falls back to the COUNT, which is still the reaches
// fact at the resolution the row alone can support and still moves when an
// installation webhook moves it.

export function ConnectedAccountCard({
  grant,
  installations = null,
  pending = null,
  installUrl = "",
  reaches = null,
  sourceNames,
  busy,
  refusal,
  remoteRevoked = null,
  onDisconnect,
}: {
  grant: CredentialRow;
  /** The installations the grant reaches, by login, when a caller knows them. */
  installations?: readonly InstallationRow[] | null;
  /** Organizations awaiting an owner's approval, when a caller knows them. */
  pending?: readonly PendingInstallation[] | null;
  installUrl?: string;
  /**
   * The control that goes and asks GitHub what this connection reaches, and
   * says when it last did.
   *
   * A SLOT RATHER THAN A HOOK, so the round trip stays the mounting group's
   * (this card owns none) while the control sits where its own sentence
   * points. It renders directly under the FACTS -- the chips and `Since` --
   * because "the count above" is the chip row and a control that dates a
   * reading belongs after the whole reading, which is the shape the picker's
   * own footer takes. Mounted after the card as a sibling it landed BELOW the
   * Disconnect block instead: a reading that dates itself, two blocks and a
   * destructive control away from the reading it dates, which is exactly what
   * `.os-refresh-row` exists to prevent (styles/index.css).
   */
  reaches?: ReactNode;
  /** The deployables that fetch under this connection, by name. */
  sourceNames: readonly string[];
  busy: boolean;
  refusal: Refusal | null;
  /** Whether the disconnect this surface just made also ended the
   *  authorization at GitHub; null while nothing has been disconnected. */
  remoteRevoked?: boolean | null;
  onDisconnect: () => void;
}) {
  const revoked = credentialIsRevoked(grant);
  const reachCount = grant.installationIds.length;
  return (
    <section className="os-field-group" aria-label="GitHub">
      <h4 className="os-subhead">GitHub</h4>

      <Chips label="GitHub connection">
        {/* THE ONE ACCENT CHIP ON THIS SURFACE. Accent is not a status
            colour here -- it names the account this cluster acts as, which
            is the single fact the card exists for. */}
        <Chip tone="accent" title="The GitHub account this cluster acts as for your sources.">
          @{grant.login || "unknown"}
        </Chip>
        {installations === null ? (
          reachCount === 0 ? null : (
            <Chip tone="muted" title="How many accounts or organizations this connection can reach.">
              {reachCount} installation{reachCount === 1 ? "" : "s"}
            </Chip>
          )
        ) : (
          installations.map((i) => (
            <Chip key={i.id} tone="muted" title={i.accountType || undefined}>
              {i.login || i.id}
            </Chip>
          ))
        )}
        {(pending ?? []).map((p) => (
          /* `--os-warn` and never `--os-error`: an organization owner has
             not clicked yet, which is somebody's next step and not a fault.
             The existing warn word (`.os-deploy-status`) rather than a new
             chip tone, because the kit's chips are neutral / accent / muted
             on purpose and a fourth would be a status colour in a
             vocabulary that deliberately has none. */
          <span key={p.login} className="os-deploy-status" data-tone="warn">
            {p.login} pending
          </span>
        ))}
        {revoked ? (
          <span className="os-deploy-status" data-tone="warn">
            disconnected
          </span>
        ) : null}
      </Chips>

      {(pending ?? []).length > 0 ? (
        /* NAMED, not "that organization". The whole content of this state is
           whom to ask, and with two pending chips above it "that" points at
           neither. */
        <Caption>
          An owner of {(pending ?? []).map((p) => p.login).join(", ")} has to approve the app before
          its repositories appear.
        </Caption>
      ) : null}

      {/* ONE FACT ROW, AND IT DOES NOT REPEAT THE CHIP. `Connected as
          @octocat` sat directly beneath an accent chip reading `@octocat`:
          the same duplication this design already removed once, when the
          `Reaches` fact was dropped because the installation chips were
          saying it (rule 7). The chip is the fact. `Since` stays, because
          nothing else says it -- as a DATE, since the minute a connection was
          made is not something anybody reads. */}
      <Facts>
        <Fact label="Since" value={formatDay(grant.createdAt)} />
      </Facts>

      {reaches}

      {reachCount === 0 && installations === null ? (
        <Caption>This connection reaches no organizations yet.</Caption>
      ) : null}

      {installUrl === "" ? null : (
        <div className="os-form-row">
          <InstallLink installUrl={installUrl} />
        </div>
      )}

      {/* THE HALF THAT DID NOT HAPPEN, and only when it did not. The engine
          revokes at GitHub first and flips this row even when that failed, so
          this cluster has stopped fetching either way -- what is left is at
          GitHub, and this is the only place that says so. `--os-warn` and not
          `--os-error`: the disconnect worked, and this is somebody's next
          step. */}
      {remoteRevoked === false ? (
        <p className="os-stop-verdict" data-tone="warn" role="status">
          This cluster has stopped fetching under it, but GitHub did not confirm the authorization was ended. Remove
          it yourself under Applications in your GitHub settings.
        </p>
      ) : null}

      {revoked ? null : (
        <Disconnect sourceNames={sourceNames} busy={busy} refusal={refusal} onDisconnect={onDisconnect} />
      )}
      {/* A revoked grant still shows what went with it, so the refusal from
          the act that revoked it has somewhere to land. */}
      {revoked && refusal ? <ProblemNotice problem={refusal} tone={toneFor(refusal.code)} /> : null}
    </section>
  );
}

/**
 * Disconnect: an armed two-step that does NOT ask for a typed name.
 *
 * ARCHIVING A PACKAGE ASKS FOR ONE; THIS DELIBERATELY DOES NOT, and the
 * difference is reversibility. Archiving is irreversible and takes every app
 * with it, so the typing is the safeguard. Disconnecting is one click to
 * undo -- Connect GitHub, and the grant is back -- so typing here would be
 * friction that teaches nothing.
 *
 * What archive really contributes is NAMING EVERY AFFECTED THING, and that
 * is what was kept: the sentence lists the deployables that fetch under this
 * connection, by name, because knowing what you are about to break is the
 * safeguard that actually applies.
 *
 * `tone="danger"` on the confirming button only. The one that arms it is an
 * ordinary control -- it changes nothing.
 *
 * IT WEARS THE SETTINGS GROUP'S GRAMMAR, NOT THE REPORT'S. This block used
 * `.os-report-part` + `.os-report-heading`, which is the deployable PAGE's
 * vocabulary: an 11px all-caps muted eyebrow, rendered here between two 13px
 * ink Subheads ("GitHub" above it, "Tokens you pasted" below) -- two heading
 * languages inside one settings group, and an all-caps label in an epic whose
 * own type rule is "no all-caps labels, no eyebrows". `.os-settings-danger`
 * keeps what the report part actually contributed -- the hairline, warm-tinted
 * -- and drops the heading.
 *
 * It also stops the control stretching. `.os-report-part` is a grid, so its
 * only child filled the column: the un-armed Disconnect measured 534px beside
 * a 71px Revoke on the same card, and the widest control on the surface was
 * the destructive one.
 *
 * AND IT CARRIES NO HEADING AT ALL. With one, a person scanning the settings
 * group read four headings at one weight -- `Sources`, `GitHub`, `Disconnect`,
 * `Tokens you pasted` -- and Disconnect is not a sibling of the connection,
 * it is part of it. Two levels, not a flat four: the warm hairline says a
 * destructive corner starts here and the button says what it does, which is
 * the whole of what the heading was adding.
 */
/**
 * A date, with no time of day.
 *
 * LOCAL TO THIS FILE, because the kit's rule is that a helper earns its place
 * there on its SECOND use. `formatMoment` is the kit's answer for a moment --
 * a deploy, a fetch, a heartbeat -- where the minute is the point; the day a
 * connection was made is not one of those, and "Aug 24, 2026, 05:00 PM" makes
 * a reader parse a timestamp to learn a date.
 */
function formatDay(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "";
  const parsed = new Date(trimmed);
  if (Number.isNaN(parsed.getTime())) return trimmed;
  return parsed.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

function Disconnect({
  sourceNames,
  busy,
  refusal,
  onDisconnect,
}: {
  sourceNames: readonly string[];
  busy: boolean;
  refusal: Refusal | null;
  onDisconnect: () => void;
}) {
  const [armed, setArmed] = useState(false);
  const named = sourceNames.join(", ");
  return (
    <section className="os-settings-danger">
      {armed ? (
        <>
          <Caption>
            {sourceNames.length === 0
              ? "Nothing fetches under this connection today."
              : `${sourceNames.length} source${sourceNames.length === 1 ? "" : "s"} fetch under this connection: ${named}.`}{" "}
            {sourceNames.length === 0
              ? "It is revoked here and at GitHub. Nothing is deleted."
              : "They will ask you to reconnect at their next fetch. Nothing is deleted."}
          </Caption>
          <div className="os-confirm-row">
            <Button tone="quiet" onClick={() => setArmed(false)}>
              Cancel
            </Button>
            <Button tone="danger" busy={busy} onClick={onDisconnect}>
              Disconnect
            </Button>
          </div>
        </>
      ) : (
        <>
          <Caption>
            Revokes this connection here and at GitHub. Your sources keep their settings and ask you to
            reconnect at their next fetch.
          </Caption>
          <Button onClick={() => setArmed(true)}>Disconnect</Button>
        </>
      )}
      {refusal ? <ProblemNotice problem={refusal} tone={toneFor(refusal.code)} /> : null}
    </section>
  );
}
