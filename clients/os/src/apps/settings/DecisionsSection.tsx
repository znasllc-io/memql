import { useState } from "react";

import { Button, Caption, Chip, Head, Measure, Notice, Refine, Select } from "../../kit";
import { formatDuration, formatMoment } from "../../kit/format";
import { useSession } from "../../chrome/access";
import { LEVELS } from "./routingFacts";
import {
  NO_FILTERS,
  billingShort,
  billingWords,
  consideredSentence,
  costFigure,
  costIsMoney,
  levelWords,
  useDecisions,
  type DecisionRow,
} from "./decisionFacts";

// Settings -> Decisions (epic memql#5153, D2).
//
// ===========================================================================
// THE ONE SECTION THAT IS GENUINELY A TABLE
// ===========================================================================
// Doors is a list of places, Levels is four sentences, Rules is an ordered
// argument. This is a LOG: many rows, read down a column, scanned for the one
// that is different. That is the shape a table is for, and it is the reason
// these four screens do not look alike -- the shape of each one is a claim
// about the question it answers.
//
// It is deliberately not a dashboard. A chart of doors-by-hour answers a
// question nobody has; "why did THIS go to a vendor" is answered by finding
// the row and opening it.
//
// ===========================================================================
// A ROW EXPANDS TO THE WALK, IN PLACE
// ===========================================================================
// DESIGN.md rule 11 -- a list and its detail never share a scroll column --
// is about a DETAIL PAGE, and this is not one: it is four more lines under
// the row, the disclosure form the Fleet call history already uses. Paging to
// a route to read three chain entries would lose the reader's place in the
// list they are scanning.

/**
 * Readable by owner, developer AND admin (D7). `{ min: "admin" }` on this
 * ladder admits admin (200), developer (300) and owner.
 *
 * A decision record carries neither the prompt nor the error message -- the
 * engine's shape omits both on purpose -- so this list answers "why did this
 * go to a vendor" while carrying nothing a reader of it should not see. That
 * omission is what makes the admin floor safe, and it is the reason the floor
 * is admin rather than developer.
 */
export const DECISIONS_SECTION_RESOURCE = "app:settings/decisions";

export function DecisionsSection() {
  const { access } = useSession();
  const [level, setLevel] = useState("");
  const [door, setDoor] = useState("");
  const [outcome, setOutcome] = useState("");
  const [rule, setRule] = useState("");
  const [open, setOpen] = useState("");

  const decisions = useDecisions(true, { ...NO_FILTERS, level, door, outcome, rule });

  const chips = [
    ...(level === "" ? [] : [{ id: "level", label: level, onRemove: () => setLevel("") }]),
    ...(door === "" ? [] : [{ id: "door", label: door, onRemove: () => setDoor("") }]),
    ...(outcome === "" ? [] : [{ id: "outcome", label: outcome, onRemove: () => setOutcome("") }]),
    ...(rule === "" ? [] : [{ id: "rule", label: `rule ${rule}`, onRemove: () => setRule("") }]),
  ];

  return (
    <div className="os-settings os-settings-wide">
      <Head
        title="Decisions"
        meta={decisions.rows.length === 0 ? undefined : `${decisions.rows.length} most recent`}
      >
        <Button onClick={decisions.reload} busy={decisions.loading} busyLabel="Reading">
          Read again
        </Button>
      </Head>
      <p className="os-caption">
        What actually happened: every call, the rule that decided it, the door it
        went through and what it cost. This is how a rule is checked.
      </p>

      {decisions.error ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.role || "your role"}.`}
          detail={decisions.error}
        />
      ) : null}

      {!decisions.supported ? (
        <Caption>
          This cluster does not record routing decisions yet. When it does, every
          call appears here with the rule that decided it.
        </Caption>
      ) : (
        <>
          <div className="os-decisions-scope">
            <Refine
              label="Refine decisions"
              search={rule}
              onSearch={setRule}
              placeholder="Rule name"
              chips={chips}
            >
              <Select id="dec-facet-level" label="Level" value={level} onChange={setLevel}>
                <option value="">Any level</option>
                {LEVELS.map((one) => (
                  <option key={one} value={one}>
                    {one}
                  </option>
                ))}
              </Select>
              <Select id="dec-facet-door" label="Door" value={door} onChange={setDoor}>
                <option value="">Any door</option>
                <option value="local">Your machines</option>
                <option value="app">Signed-in apps</option>
                <option value="federation">A paid vendor</option>
              </Select>
              <Select id="dec-facet-outcome" label="Outcome" value={outcome} onChange={setOutcome}>
                <option value="">Any outcome</option>
                <option value="ok">Served</option>
                <option value="error">Failed</option>
                <option value="cancelled">Cancelled</option>
              </Select>
            </Refine>
          </div>

          {decisions.loading && decisions.rows.length === 0 ? (
            <Caption>Reading the decisions.</Caption>
          ) : decisions.rows.length === 0 ? (
            <Caption>
              {chips.length > 0
                ? "No decision matches that."
                : "No calls have been routed yet. The first one appears here."}
            </Caption>
          ) : (
            <ul className="os-decisions" aria-label="Recent routing decisions">
              {decisions.rows.map((row) => (
                <DecisionLine
                  key={row.id || row.requestId}
                  row={row}
                  open={open === (row.id || row.requestId)}
                  onOpen={() =>
                    setOpen(open === (row.id || row.requestId) ? "" : row.id || row.requestId)
                  }
                />
              ))}
            </ul>
          )}
        </>
      )}
    </div>
  );
}

/** Time of day, or the raw value when it will not parse. Never invented. */
function clockOf(value: string): string {
  if (value === "") return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return value;
  return at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

const DOOR_WORD: Record<string, string> = {
  local: "your machines",
  app: "a signed-in app",
  federation: "a paid vendor",
};

/**
 * One decision.
 *
 * THE COST COLUMN NEVER PRINTS A ZERO FOR A FREE CALL. A call served by a
 * machine the person owns cost this cluster nothing, and "$0.00" claims a
 * measurement was taken and came out zero -- which would also make a free call
 * and a mis-priced metered one identical. `costFigure` hands an absent figure
 * carrying the reason, and `Measure` draws the absence.
 */
function DecisionLine({
  row,
  open,
  onOpen,
}: {
  row: DecisionRow;
  open: boolean;
  onOpen: () => void;
}) {
  const level = levelWords(row);
  const doorWord = DOOR_WORD[row.door] ?? row.door;
  const spoken = [
    formatMoment(row.createdAt),
    row.promptName,
    level,
    doorWord,
    row.model,
    row.outcome === "ok" ? "served" : row.outcome,
  ]
    .filter((p) => p !== "")
    .join(", ");

  return (
    <li className="os-decision-item">
      <button
        type="button"
        className="os-decision"
        data-os-door={row.door}
        data-os-outcome={row.outcome}
        data-open={open || undefined}
        aria-expanded={open}
        aria-label={spoken}
        onClick={onOpen}
      >
        {/* THE TIME, not the date. A log is scanned for "the one a minute
            ago"; the full moment is one hover away and takes no column. */}
        <span className="os-decision-when os-mono" title={formatMoment(row.createdAt)}>
          {clockOf(row.createdAt)}
        </span>
        <span className="os-decision-prompt">{row.promptName || "a call"}</span>
        <span className="os-decision-level">
          {level}
          {row.degraded ? <Chip tone="muted">degraded</Chip> : null}
        </span>
        <span className="os-decision-door">{doorWord}</span>
        <span className="os-decision-model os-mono">{row.model}</span>
        <span className="os-decision-cost os-mono">
          {costIsMoney(row) ? (
            <Measure figure={costFigure(row)} format={(v) => `$${v.toFixed(4)}`} />
          ) : (
            <span className="os-decision-free" title={billingWords(row)}>
              {billingShort(row)}
            </span>
          )}
        </span>
        <span className="os-decision-took os-mono">
          {row.totalDurationMs.kind === "measured"
            ? formatDuration(row.totalDurationMs.value)
            : ""}
        </span>
      </button>

      {open ? (
        <div className="os-decision-detail">
          <p className="os-decision-detail-line">
            {row.rule === ""
              ? "No rule was recorded for this call."
              : `Decided by ${row.rule}, which chose ${row.policy || "no policy"}.`}
          </p>
          <p className="os-decision-detail-line">{consideredSentence(row)}</p>
          {row.considered.length === 0 ? null : (
            <ol className="os-decision-walk" aria-label="What the chain looked at">
              {row.considered.map((c, i) => (
                <li key={`${c.entry}-${i}`} data-os-served={c.served || undefined}>
                  <span className="os-mono">{c.entry}</span>
                  <span className="os-decision-walk-door">{DOOR_WORD[c.door] ?? c.door}</span>
                  <span className="os-decision-walk-why">
                    {c.served ? "served this call" : c.why || "passed over"}
                  </span>
                </li>
              ))}
            </ol>
          )}
          {row.touches.length === 0 ? null : (
            <p className="os-decision-detail-line">Touched {row.touches.join(", ")}.</p>
          )}
          {row.executionSurface === "" ? null : (
            <p className="os-decision-detail-line os-mono">{row.executionSurface}</p>
          )}
        </div>
      ) : null}
    </li>
  );
}
