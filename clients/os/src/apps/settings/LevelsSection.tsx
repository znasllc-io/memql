import { Caption, RecordList, RecordRow, Head, Measure, Notice } from "../../kit";
import { useSession } from "../../chrome/access";
import { doorFor } from "./providerFacts";
import { useProviderRegistry } from "./providerFacts";
import {
  EMBEDDINGS_NEVER_DEGRADES,
  doorReadings,
  levelReadings,
  useInferenceStatus,
  useLevelBindings,
  type LevelReading,
} from "./routingFacts";

// Settings -> Levels (epic memql#5153, D1).
//
// ===========================================================================
// THIS SECTION HAS NO ACTS, AND THAT IS THE DESIGN
// ===========================================================================
// A level is not a setting. It is what a call ASKS FOR -- "this needs
// reasoning" -- and what it gets is decided by the rules. So this page is a
// mirror: it answers "if something asked for `strong` right now, what would
// happen?", and the way to change the answer is to open a door or write a
// rule. Putting a control here would offer to edit a reflection.
//
// ===========================================================================
// WHY ROWS THAT READ AS SENTENCES, AND NOT A TABLE
// ===========================================================================
// A table of level / model / machine / measured is the obvious form and it is
// wrong here, for a reason that only shows up on a fresh cluster: a level with
// no local door leaves three cells to fill, and the mark you fill them with is
// a dash -- the same mark a zero-ish absence uses everywhere else in this
// shell. Four levels with nine dashes between them reads as a broken screen
// rather than as a cluster that has not pulled a model yet.
//
// Written as a sentence, the same row says "Nothing local and no signed-in
// app, so this goes to Anthropic and is billed" -- which is not a gap at all,
// it is the answer, and it is the one a person can act on. The gap is the
// content. (The owner picked this form over the table on 2026-09-07.)
//
// ===========================================================================
// WHAT THIS PAGE WILL NOT INVENT
// ===========================================================================
// Which MODEL a level resolves to is the engine's binding (epic memql#5127).
// Until that read exists, a row says which DOOR takes it -- honestly derivable
// from the doors' own state -- and says the fleet picks the model. It does not
// rank the fleet's models itself and present the guess as an answer. A
// plausible mechanism rendered in the same ink as a measurement is the failure
// this whole epic's `Measure` piece exists to prevent, and it would be worse
// here, because a level is exactly the thing a person would then act on.

/**
 * Readable by owner, developer AND admin (D7).
 *
 * `{ min: "admin" }` on this ladder admits admin (200), developer (300) and
 * owner -- so the floor is admin and the set is all three. An admin answering
 * "why did this go to a vendor" needs to see what the levels resolve to, and
 * this page carries no prompt content and no credential.
 */
export const LEVELS_SECTION_RESOURCE = "app:settings/levels";

export function LevelsSection() {
  const { access } = useSession();
  const inference = useInferenceStatus(true);
  const registry = useProviderRegistry(true);
  const bindings = useLevelBindings(true);

  const doors = doorReadings(inference.status, (vendor) => doorFor(vendor, registry.rows));
  const levels = levelReadings(doors, bindings.bound);
  const anyOpen = doors.some((d) => d.state === "open");

  // SAY IT ONCE (DESIGN.md rule 7), AND THE RULE IS "DOMINANT", NOT "ALL FOUR".
  //
  // Three states of this page were repeating themselves. With no engine
  // binding every level resolves through the same door and all four rows said
  // the same sentence. With no door open at all, three said one thing and
  // `embeddings` said its own -- so an "all four agree" test did not fire, and
  // the page printed one sentence three times, its advice three times, and
  // then a Notice saying it a fourth.
  //
  // So the sentence shared by MOST levels is stated once above the list, and a
  // row prints its own only where it DIFFERS. The rows that differ are then
  // the only sentences on the page, which is what a person came to find.
  //
  // When nothing is open at all the Notice below carries the whole message and
  // the shared line stands down: a warn rule and a paragraph saying the same
  // thing is the repetition wearing two weights.
  const counts = new Map<string, number>();
  for (const l of levels) counts.set(l.sentence, (counts.get(l.sentence) ?? 0) + 1);
  let dominant = "";
  let best = 1;
  for (const [sentence, n] of counts) {
    if (n > best) {
      best = n;
      dominant = sentence;
    }
  }
  const noticeCarriesIt = inference.status.read && !anyOpen;
  const shared = noticeCarriesIt ? "" : dominant;
  const sharedAdvice =
    shared === ""
      ? ""
      : (levels.find((l) => l.sentence === shared)?.advice ?? "");

  return (
    <div className="os-settings">
      <Head title="Levels" meta={inference.status.read && !inference.status.error ? levels.length : undefined} />
      <p className="os-caption">
        A level is how much intelligence a call needs. Every call names one and
        never names a model, so a model can change without a release. What each
        level gets is decided by the rules -- this page is what those rules
        currently produce.
      </p>

      {inference.status.error ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.role || "your role"}.`}
          next="Without it these rows cannot say which door takes a call."
          detail={inference.status.error}
        />
      ) : null}

      {!inference.status.read && inference.status.error === "" ? (
        <Caption>Asking the cluster.</Caption>
      ) : (
        <>
          {shared === "" ? null : (
            <>
              <p className="os-levels-shared">{shared}</p>
              {sharedAdvice === "" ? null : (
                <p className="os-level-advice os-levels-shared-advice">{sharedAdvice}</p>
              )}
            </>
          )}
          <RecordList as="ul" label="Levels">
            {levels.map((level) => (
              <LevelRow
                key={level.id}
                level={level}
                shared={shared}
                sharedAdvice={sharedAdvice}
                suppressAll={noticeCarriesIt}
              />
            ))}
          </RecordList>
        </>
      )}

      {inference.status.read && !anyOpen ? (
        <Notice
          tone="warn"
          sentence="No door is open, so every level parks."
          next="Pull a model onto a machine you own, sign in to Claude Code or Codex on one, or federate a vendor in Doors."
        />
      ) : null}

      {bindings.available ? null : (
        <Caption>
          Which model serves each level is the fleet&apos;s to choose, and this
          cluster does not report it yet. The door each level goes through is
          what these rows are telling you.
        </Caption>
      )}
    </div>
  );
}

/**
 * One level, as a sentence.
 *
 * The level's own name is the only thing set in the strong ink -- it is the
 * word a person carries away and the one they will type into a rule. The
 * meaning sits under it in the quiet ink, because it teaches once and is then
 * furniture. The answer is the sentence.
 *
 * `advice` renders ONLY where there is something to do. A cluster whose fleet
 * takes every level gets four rows and no advice at all, which is what a
 * working system should look like: advice attached to a working state is
 * furniture that never goes away.
 */
function LevelRow({
  level,
  shared,
  sharedAdvice,
  suppressAll,
}: {
  level: LevelReading;
  /** The sentence already said once above; a row repeats nothing. */
  shared: string;
  /** Advice already said once above; a row repeats nothing. */
  sharedAdvice: string;
  /** The Notice below is carrying the whole message; rows say only their own note. */
  suppressAll: boolean;
}) {
  const metered = level.door !== null && level.door.metered;
  return (
    <div data-os-level={level.id} data-os-metered={metered || undefined}>
      <RecordRow name={level.id} secondary={level.meaning}>
        <span>
        {level.sentence === shared || suppressAll ? null : (
          <p className="os-level-said">{level.sentence}</p>
        )}
        {level.id === "embeddings" ? (
          <p className="os-level-note">{EMBEDDINGS_NEVER_DEGRADES}</p>
        ) : null}
        {level.measured.kind === "measured" ? (
          <p className="os-level-measured">
            Measured <Measure figure={level.measured} suffix=" structured" />
          </p>
        ) : null}
        {level.advice === "" || level.advice === sharedAdvice || suppressAll ? null : (
          <p className="os-level-advice">{level.advice}</p>
        )}
        </span>
      </RecordRow>
    </div>
  );
}
