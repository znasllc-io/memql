import { useMemo, useState } from "react";
import { RefreshCw } from "lucide-react";

import {
  Button,
  Caption,
  Chip,
  Head,
  Notice,
  Panel,
  Refine,
  Row as KitRow,
  formatFreshness,
  formatMoment,
  useNow,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import {
  automationMatches,
  rung,
  rungMeaning,
  rungWord,
  statusMeaning,
  statusWord,
  type AutomationRow,
} from "./automations";
import { useAutomations, useSetAutomationStatus } from "./useAutomations";
import { idTail } from "./rows";

// AUTOMATIONS: what this instance can replay without a model.
//
// ===========================================================================
// THIS IS WHERE THE PRODUCT'S CLAIM BECOMES CHECKABLE
// ===========================================================================
// Every other surface in this app is about one piece of work. This one is
// about what the instance has LEARNED: the templates a goal compiled to, how
// well each has done, and whether it is armed. A person who wants to know
// whether the system is actually getting cheaper reads this list.
//
// ===========================================================================
// IT IS A READ AND IT SAYS WHEN IT LOOKED
// ===========================================================================
// `v1:authoring:construct` carries no broadcast routing rule, so there is
// nothing to subscribe to -- see useAutomations.ts, which records the check
// rather than the assumption. A live-looking list that silently never moves is
// worse than a read that dates itself.
//
// ===========================================================================
// THE LADDER IS A WORD, NEVER A PERCENTAGE
// ===========================================================================
// `reliability` is 0..1 and it is NOT a probability of success: it climbs when
// a run whose fingerprints matched succeeds and decays on mismatch and on
// disuse. "62%" invites a reader to treat it as odds. The rung a person
// actually cares about is whether this can be trusted to run unwatched, and
// that is a word -- with "not yet proven" kept distinct from "struggling",
// because a template nobody has run has earned nothing and one that has been
// run and kept missing has earned less than nothing.

export interface AutomationsSectionProps {
  selectedId: string;
  onSelect: (constructId: string) => void;
}

export function AutomationsSection({ selectedId, onSelect }: AutomationsSectionProps) {
  const now = useNow(30_000);
  const [search, setSearch] = useState("");
  const catalog = useAutomations();
  const status = useSetAutomationStatus();

  const rows = useMemo(
    () =>
      catalog.automations
        .filter((automation) => automationMatches(automation, search))
        // Armed first, then by how far each has climbed, then by name. A
        // total order: two templates with the same rung and the same name
        // would otherwise swap places between reads.
        .sort((a, b) => {
          const armed = Number(b.status === "active") - Number(a.status === "active");
          if (armed !== 0) return armed;
          if (a.reliability !== b.reliability) return b.reliability - a.reliability;
          return a.name.localeCompare(b.name);
        }),
    [catalog.automations, search],
  );

  const selected = rows.find((row) => idTail(row.id) === idTail(selectedId)) ?? null;

  const acts: Act[] = [];
  if (selected !== null) {
    // AN ILLEGAL ACT IS ABSENT (rule 12). Arm is offered on anything that is
    // not already armed; Retire only on something that is. Neither is rendered
    // greyed out, because a control that cannot be used is a question about
    // why.
    if (selected.status !== "active") {
      acts.push({
        label: "Arm it",
        tone: "primary",
        busy: status.busy === selected.id,
        ariaLabel: `Arm ${selected.name}: register it in the shared runtime so a matching goal replays it without a model`,
        onAct: () => {
          void status.set(selected.id, "active").then((ok) => {
            if (ok) catalog.read();
          });
        },
      });
    }
    if (selected.status === "active") {
      acts.push({
        label: "Retire it",
        tone: "danger",
        busy: status.busy === selected.id,
        ariaLabel: `Retire ${selected.name}: it stays readable so runs that used it can be explained, and is never selected again`,
        onAct: () => {
          void status.set(selected.id, "retired").then((ok) => {
            if (ok) catalog.read();
          });
        },
      });
    }
  }

  const tone: ActionBarTone =
    selected === null ? "none" : selected.status === "active" ? "live" : "paused";

  return (
    <div className="os-nexus-automations">
      <Head
        title="Automations"
        meta={`${rows.length} ${rows.length === 1 ? "automation" : "automations"}`}
      >
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Name or namespace"
          label="Search your automations"
        />
        <Button onClick={catalog.read} busy={catalog.state === "loading"}>
          <RefreshCw size={13} aria-hidden />
          Look again
        </Button>
      </Head>

      <div className="os-nexus-split">
        <div className="os-nexus-column">
          {catalog.error !== "" ? (
            <Notice
              tone="error"
              sentence="The catalog could not be read."
              next="Look again once the cluster answers."
              detail={catalog.error}
            />
          ) : null}

          <ul className="os-nexus-catalog" aria-label="Automations this instance can replay">
            {rows.map((automation) => (
              <li key={automation.id}>
                <KitRow
                  name={automation.name}
                  current={idTail(automation.id) === idTail(selectedId)}
                  onOpen={() => onSelect(automation.id)}
                  state={
                    <>
                      <Chip
                        tone={automation.status === "active" ? "accent" : "muted"}
                        title={statusMeaning(automation.status)}
                      >
                        {statusWord(automation.status)}
                      </Chip>
                      <RungMark automation={automation} />
                    </>
                  }
                >
                  <span className="os-nexus-row-sub os-mono">
                    {automation.targetNamespace === "" ? "—" : automation.targetNamespace}
                  </span>
                </KitRow>
              </li>
            ))}
          </ul>

          {rows.length === 0 ? (
            <Caption>
              {catalog.state === "loading"
                ? "Reading the catalog"
                : search.trim() !== ""
                  ? "No automation here matches that."
                  : // AN EMPTY SCREEN IS AN INVITATION, and here the invitation
                    // is not a button -- nobody authors an automation by hand in
                    // this app. It says where they come from instead.
                    "Nothing here yet. An automation appears when a goal is worked out: the system compiles what it decided into a template, and a template that keeps succeeding earns its way up this list."}
            </Caption>
          ) : null}

          <Caption>
            {catalog.readAt === ""
              ? "Not read yet."
              : `Read ${formatFreshness(catalog.readAt, now)}.`}{" "}
            This list is not live -- the catalog broadcasts nothing, so it is a read that dates
            itself rather than a list that would silently never move.
            {catalog.bounded
              ? ` Showing the first ${catalog.scanned} catalogued constructs; there may be more.`
              : ""}
          </Caption>
        </div>

        {selected === null ? null : (
          <div className="os-nexus-column os-nexus-detail">
            <Panel label={selected.name}>
              <p className="os-nexus-rung-line">
                <strong>{rungWord(rung(selected))}</strong> — {rungMeaning(selected)}
              </p>
              <dl className="os-facts">
                <dt>Registers as</dt>
                <dd className="os-mono">
                  {selected.targetNamespace === "" ? "—" : selected.targetNamespace}
                </dd>
                <dt>Status</dt>
                <dd title={statusMeaning(selected.status)}>{statusWord(selected.status)}</dd>
                <dt>Successful runs</dt>
                <dd className="os-mono">{selected.reinforceCount}</dd>
                <dt>Last succeeded</dt>
                <dd>
                  {selected.lastReinforced === ""
                    ? "never"
                    : formatFreshness(selected.lastReinforced, now)}
                </dd>
                <dt>In the catalog since</dt>
                <dd>
                  {selected.catalogedAt === ""
                    ? "—"
                    : formatMoment(selected.catalogedAt)}
                </dd>
                <dt>Compiled from a goal</dt>
                <dd>
                  {/* The signature is a hash, so it is shown as PRESENT or
                      ABSENT rather than printed: sixty-four hex characters
                      tell a reader nothing, and the fact they want is whether
                      this template answers a goal shape at all. */}
                  {selected.goalSignature === ""
                    ? "no — authored directly"
                    : "yes — it answers a goal shape"}
                </dd>
              </dl>
            </Panel>

            {selected.source === "" ? null : (
              <Panel label="What it does">
                <pre className="os-nexus-source os-mono">{selected.source}</pre>
              </Panel>
            )}
          </div>
        )}
      </div>

      <ActionBar
        state={selected === null ? "Nothing selected" : statusWord(selected.status)}
        detail={
          selected === null
            ? "select an automation to arm or retire it"
            : statusMeaning(selected.status)
        }
        tone={tone}
        acts={acts}
      >
        {status.error === "" ? null : (
          <span className="os-nexus-act-error os-mono" role="alert">
            {status.error}
          </span>
        )}
      </ActionBar>
    </div>
  );
}

/**
 * The ladder, as a mark.
 *
 * FIVE RUNGS DRAWN AS FIVE TICKS, filled to where this template stands. It is
 * a shape rather than a colour, so it survives greyscale and every theme pack,
 * and the accessible name carries the word -- a reader who cannot see the
 * ticks gets "Proven", which is the whole reading.
 */
function RungMark({ automation }: { automation: AutomationRow }) {
  const level = rung(automation);
  const filled =
    level === "proven" ? 4 : level === "good" ? 3 : level === "fair" ? 2 : level === "poor" ? 1 : 0;
  return (
    <span
      className="os-nexus-rung"
      data-rung={level}
      role="img"
      aria-label={`${rungWord(level)}. ${rungMeaning(automation)}`}
      title={rungMeaning(automation)}
    >
      {[0, 1, 2, 3].map((i) => (
        <span key={i} className="os-nexus-rung-tick" data-on={i < filled || undefined} />
      ))}
    </span>
  );
}
