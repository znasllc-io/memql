import { useState } from "react";

import { Button, Caption, Chip, Chips, Fact, Facts } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import type { Refusal } from "../packages/actions";
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
  sourceNames,
  busy,
  refusal,
  onDisconnect,
}: {
  grant: CredentialRow;
  /** The installations the grant reaches, by login, when a caller knows them. */
  installations?: readonly InstallationRow[] | null;
  /** Organisations awaiting an owner's approval, when a caller knows them. */
  pending?: readonly PendingInstallation[] | null;
  installUrl?: string;
  /** The deployables that fetch under this connection, by name. */
  sourceNames: readonly string[];
  busy: boolean;
  refusal: Refusal | null;
  onDisconnect: () => void;
}) {
  const revoked = credentialIsRevoked(grant);
  const reaches = grant.installationIds.length;
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
          reaches === 0 ? null : (
            <Chip tone="muted" title="How many accounts or organisations this connection can reach.">
              {reaches} installation{reaches === 1 ? "" : "s"}
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
          /* `--os-warn` and never `--os-error`: an organisation owner has
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
        <Caption>
          An owner of that organisation has to approve the app before its repositories appear.
        </Caption>
      ) : null}

      <Facts>
        <Fact label="Connected as" value={grant.login === "" ? "" : `@${grant.login}`} />
        <Fact label="Since" value={formatMoment(grant.createdAt)} />
      </Facts>

      {reaches === 0 && installations === null ? (
        <Caption>This connection reaches no organisations yet.</Caption>
      ) : null}

      {installUrl === "" ? null : (
        <div className="os-form-row">
          <InstallLink installUrl={installUrl} />
        </div>
      )}

      {revoked ? null : (
        <Disconnect sourceNames={sourceNames} busy={busy} refusal={refusal} onDisconnect={onDisconnect} />
      )}
      {/* A revoked grant still shows what went with it, so the refusal from
          the act that revoked it has somewhere to land. */}
      {revoked && refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
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
 */
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
    <section className="os-report-part os-danger-part">
      <h4 className="os-report-heading">Disconnect</h4>
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
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
    </section>
  );
}
