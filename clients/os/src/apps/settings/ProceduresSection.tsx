import { Fact, Facts, Head, Notice, Panel, Subhead } from "../../kit";
import { InfoDetail } from "../../kit/InfoDetail";
import { useSession } from "../../chrome/access";
import type { LadderPolicy } from "../nexus/ladder";
import { useLadderPolicy } from "./useLadderPolicy";

// Settings -> Procedures (epic memql#5408, #5412).
//
// ===========================================================================
// THE LADDER'S VALUES, SAID AS WHAT THEY DO
// ===========================================================================
// A learned procedure climbs candidate -> shadow -> canary -> trusted on its
// own evidence, and six numbers decide how much evidence each step takes.
// They are the cluster's ROWS (v1:authoring:ladderPolicy:primary), so a
// person reading why a procedure has not been promoted can look up the rule
// it is being held to -- each one as the sentence it enforces, with its unit,
// rather than as a bare field name and a number.
//
// ===========================================================================
// NO ACTS, AND THE INFORMATION CONTROL SAYS WHY
// ===========================================================================
// The seed writes the row again on every boot, so a value edited anywhere but
// the seed would not survive the next restart. Offering a field here would be
// offering a change that silently reverts; the values move with a deploy. That
// is general guidance rather than something to decide now, so it lives behind
// the section's information control with the ladder itself (DESIGN.md rule 7),
// and the page is the values.
//
// ===========================================================================
// AN ABSENT ROW IS SAID, NEVER FILLED IN
// ===========================================================================
// The Go side falls back to its own defaults when the row is missing. This
// page does not: a number shown in Settings is a claim that the cluster holds
// it, and a cluster that has not published its policy holds none.

/**
 * Readable by owner, developer and admin -- the Levels section's roles (the
 * seeded `read app:settings/procedures` rows mirror `app:settings/levels`).
 * The policy row itself is readable from the reader rung up; the section is
 * cluster configuration, and it sits with the other AI sections.
 */
export const PROCEDURES_SECTION_RESOURCE = "app:settings/procedures";

function times(n: number | null, one: string, many: string): string {
  if (n === null) return "";
  return `${n} ${n === 1 ? one : many}`;
}

export function ProceduresSection() {
  const { access } = useSession();
  const read = useLadderPolicy();

  return (
    <div className="os-settings os-settings-procedures">
      <Head title="Procedures" />

      {read.state === "error" ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.role || "your role"}.`}
          detail={read.error}
        />
      ) : null}

      <Panel label="Certification ladder">
        <div className="os-record-heading">
          <Subhead>Certification ladder</Subhead>
          <InfoDetail title="The certification ladder">
            <p>
              A learned procedure is work the system watched an app do more than once, turned into
              steps it can replay without a model. It starts as a candidate. In shadow it replays
              beside the app, which still answers, and every step is compared with what the app
              did.
            </p>
            <p>
              When it has matched enough times, across enough different values, promotion is put
              to you once. Approved, it runs for real as a canary with the app standing by, and
              clean replays make it trusted to run without a model.
            </p>
            <p>
              Failed replays demote it back to shadow without asking, and a procedure left unused
              retires. A changed procedure starts again as a new candidate.
            </p>
            <p>
              These values are the cluster&apos;s seeded policy. The seed writes them again on every
              boot, so they change with a deploy rather than here; a cluster whose engine has not
              written them yet uses built-in values it does not publish.
            </p>
          </InfoDetail>
        </div>
        {read.state === "loading" ? (
          // THE SHAPE OF THE WAIT IS SILENT (DESIGN.md, "Loading is the shape
          // of the content, never a message"): announced, never painted.
          <p className="os-sr-only" role="status" aria-busy="true">
            Loading the ladder&apos;s values
          </p>
        ) : read.state === "error" ? null : read.policy === null ? (
          // STATED WITHOUT THE NUMBERS. The engine does fall back to values of
          // its own; printing them would present its fallback as this
          // cluster's policy.
          <p className="os-settings-procedures-absent">
            This cluster has not published its ladder policy yet, so no values are shown.
          </p>
        ) : (
          <PolicyFacts policy={read.policy} />
        )}
      </Panel>
    </div>
  );
}

function PolicyFacts({ policy }: { policy: LadderPolicy }) {
  return (
    <Facts>
      <Fact
        label="Shadow matches before promotion is proposed"
        value={times(policy.shadowMatches, "in a row", "in a row")}
      />
      <Fact label="Distinct bindings each parameter needs" value={times(policy.distinctBindings, "binding", "bindings")} />
      <Fact
        label="Clean canary replays before it is trusted"
        value={times(policy.canaryMatches, "in a row", "in a row")}
      />
      <Fact label="Failed replays that demote it" value={times(policy.failuresToDemote, "in a row", "in a row")} />
      <Fact
        label="Failures after passing checks that demote it"
        value={times(policy.insufficientToDemote, "replay", "replays")}
      />
      <Fact label="Unused this long, it retires" value={times(policy.retireAfterDays, "day", "days")} />
    </Facts>
  );
}
