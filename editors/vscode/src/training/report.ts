// The words: what a training action asks before it acts, and what it says after.
//
// Pure text, no `vscode` -- so every sentence a developer is shown before
// committing something durable to a cluster is unit-testable, and so the modal
// adapter in extension.ts holds no wording of its own.
//
// THREE RULES RUN THROUGH ALL OF IT.
//
//  1. NAME THE THING AND NAME THE CLUSTER. "Are you sure?" tells a developer
//     nothing they can check; "Promote spaceParticipants to staging" is a claim
//     they can recognise as right or wrong in a second. This is the run path's
//     writeConfirmationMessage doctrine, applied to a strictly larger act.
//
//  2. RENDER FROM THE STRUCTURED FIELDS, never from the engine's prose. A
//     concept diff arrives classified -- field, kind, was, now, row count,
//     referencing constructs -- and `summary` carries the engine's own rendered
//     block for the cases where its exact wording is wanted. Rebuilding the
//     classification here would be a second authority on what is breaking, and
//     the two would disagree the first time the engine's rules moved.
//
//  3. A COUNT NOBODY TOOK IS NOT ZERO. `rowsAffected` means nothing unless
//     `rowCountKnown` is true: a node with no database cannot count, and
//     rendering its zero reads as "this is safe" at exactly the moment nothing
//     is known. So the unknown case says so in words rather than falling back to
//     a number.
//
// Refs: #3763 #3745

import type {
  ConceptSchemaChange,
  ConceptSchemaDiff,
  DemoteOutcome,
} from "@znasllc-io/memql-sdk-core/authoring";
import { DemoteOutcomeRemoved, DemoteOutcomeRetired } from "@znasllc-io/memql-sdk-core/authoring";

import type { ClosureMember, TrainingBundle } from "./closure.js";

/** What the adapter turns into a modal. */
export interface TrainingPrompt {
  /** The headline: one sentence naming the act, the construct and the cluster. */
  message: string;
  /** The body: the closure, the consequences, the diff. Several lines. */
  detail: string;
  /** The affirmative button. It NAMES THE ACT -- never "OK", never "Yes". */
  confirmLabel: string;
}

/** The cluster facts a prompt needs. Never the credential. */
export interface TrainingCluster {
  name: string;
  label: string;
  /** True only when clusters.yaml marks it `local: true`. Absent means NOT local. */
  local: boolean;
  /**
   * The recorded release (memql#3990), when clusters.yaml carries one.
   *
   * Carried here so a failure that severed the session can say whether this
   * cluster is OLDER than the plugin -- see `version/skewHint.ts`. Undefined is
   * the ordinary case for a cluster nothing has learned a version for yet, and
   * produces no hint rather than a guess.
   */
  version?: string;
}

// -----------------------------------------------------------------------------
// The closure, in words
// -----------------------------------------------------------------------------

/**
 * closureLines renders what a bundle is about to carry, one line per file.
 *
 * SHOWS THE INCLUDED FILES AND THE UNCLASSIFIED ONES, and omits the files the
 * cluster already has. The developer is being asked to approve what is being
 * committed, so the list is of what is going -- with one exception: a file the
 * language server could not classify was left OUT of the bundle on an
 * assumption, and an assumption made on their behalf is exactly the thing they
 * need to see.
 *
 * `display` maps an absolute path to whatever the caller wants shown -- a
 * workspace-relative path in the editor, the raw path in a test.
 */
function closureLines(bundle: TrainingBundle, display: (path: string) => string): string[] {
  const lines: string[] = [];
  for (const member of bundle.members) {
    if (!member.included) continue;
    lines.push(`${display(member.path)}${member.reason === "active" ? "" : "  (dependency)"}`);
    for (const c of member.constructs) {
      // Every construct in an included file goes to the engine, whatever its
      // state, because a bundle is compiled and promoted whole. Listing the
      // trained ones too is what keeps this an honest inventory rather than a
      // flattering one.
      lines.push(`    ${c.state.padEnd(9)} ${c.kind} ${c.name}`);
    }
  }
  const unclassified = bundle.members.filter((m) => m.reason === "unclassified");
  if (unclassified.length > 0) {
    lines.push("");
    lines.push(
      "Left out, because the language server could not say what state they are in:",
    );
    for (const member of unclassified) lines.push(`    ${display(member.path)}`);
  }
  return lines;
}

/** How many constructs a bundle actually submits. What a dry-run reports. */
export function constructCount(bundle: TrainingBundle): number {
  return bundle.members
    .filter((m) => m.included)
    .reduce((total, m) => total + m.constructs.length, 0);
}

/** The dependency files a bundle carries beyond the active one. */
function dependencyMembers(bundle: TrainingBundle): ClosureMember[] {
  return bundle.members.filter((m) => m.included && m.reason !== "active");
}

// -----------------------------------------------------------------------------
// The three confirmations
// -----------------------------------------------------------------------------

/**
 * sessionPrompt: the confirmation for Try in session.
 *
 * IT EXISTS TO SAY "TEMPORARY". A session-define and a promote look identical
 * from the outside -- the construct becomes callable by name, immediately -- and
 * the difference between them is the whole design. So the confirmation's job is
 * not to guard against the act (it commits nothing and vanishes on its own) but
 * to make sure nobody believes they have promoted something.
 */
export function sessionPrompt(
  cluster: TrainingCluster,
  name: string,
  bundle: TrainingBundle,
  display: (path: string) => string,
): TrainingPrompt {
  const detail = [
    `"${name}" and everything in this bundle become callable by name on ${cluster.label}, for this connection only.`,
    "",
    "TEMPORARY. Nothing is persisted, nothing is visible to anyone else, and every definition is dropped the moment the connection drops or you switch cluster. Promote is what makes a construct outlive the session.",
    "",
    "This bundle carries:",
    ...closureLines(bundle, display),
  ].join("\n");
  return {
    message: `Define "${name}" on ${cluster.label} for this session only?`,
    detail,
    confirmLabel: "Define for this session",
  };
}

/**
 * promotePrompt: the confirmation for Promote.
 *
 * SHOWS THE CLOSURE FIRST. The developer clicked a lens above one construct, and
 * what actually goes is that construct plus every dependency the cluster does
 * not already have -- which is the one fact about a promote that is not visible
 * from where the click happened.
 *
 * A NON-LOCAL CLUSTER IS SAID OUT LOUD, not made into a second dialog. Browsing
 * a remote cluster is not a quieter way to write to it (memql#3309 set that for
 * runs, and a promote is larger), but the friction that works is a sentence
 * naming staging in a modal already on screen, not a second modal nobody reads.
 */
export function promotePrompt(
  cluster: TrainingCluster,
  name: string,
  bundle: TrainingBundle,
  display: (path: string) => string,
): TrainingPrompt {
  const dependencies = dependencyMembers(bundle);
  const detail = [
    ...(cluster.local
      ? []
      : [
          `${cluster.label} is not marked local in clusters.yaml. This writes to a shared cluster: the constructs are persisted, every session on it can call them, and a restart replays them.`,
          "",
        ]),
    dependencies.length === 0
      ? "This promotes:"
      : `This promotes "${name}" together with ${dependencies.length} dependency file(s) the cluster does not have. All of it goes, or none of it does:`,
    ...closureLines(bundle, display),
    "",
    "Promote is owner-only. If you are not the cluster owner the engine refuses and says so.",
  ].join("\n");
  return {
    message: `Promote "${name}" to ${cluster.label}?`,
    detail,
    confirmLabel: "Promote",
  };
}

/**
 * stagePrompt: the confirmation for Stage.
 *
 * SAYS WHO CAN CALL IT, and that is the whole of what distinguishes this modal
 * from the promote one. Staging is durable -- persisted, replayed at boot,
 * surviving the connection -- which is most of what a developer reads "this is
 * real now" from, so the sentence that has to land is the OTHER half: nobody
 * else on this cluster can call it until it is trained.
 *
 * SHOWS THE CLOSURE, for promotePrompt's reason. A staged construct still has to
 * bind, so a dependency the cluster does not have goes with it or the stage
 * lands something that cannot resolve.
 *
 * NO OWNER-ONLY SENTENCE. Staging takes the same owner-or-developer bar as Try
 * in session, so telling a developer they may be refused would be wrong. It says
 * what staging is instead.
 */
export function stagePrompt(
  cluster: TrainingCluster,
  name: string,
  bundle: TrainingBundle,
  display: (path: string) => string,
): TrainingPrompt {
  const dependencies = dependencyMembers(bundle);
  const detail = [
    `"${name}" becomes durable on ${cluster.label}: persisted, replayed when the cluster restarts, and callable BY YOU AND BY NOBODY ELSE until you train it.`,
    "",
    dependencies.length === 0
      ? "This stages:"
      : `This stages "${name}" together with ${dependencies.length} dependency file(s) the cluster does not have. All of it goes, or none of it does:`,
    ...closureLines(bundle, display),
    "",
    "A concept cannot be staged. If the closure declares one the engine refuses the whole bundle and names it -- train the concept, then stage the constructs bound to it.",
  ].join("\n");
  return {
    message: `Stage "${name}" on ${cluster.label}?`,
    detail,
    confirmLabel: "Stage",
  };
}

/**
 * demotePrompt: the confirmation for Demote.
 *
 * STATES THE CONCEPT RULE UNCONDITIONALLY, without trying to predict which side
 * of it this demote lands on. Only the engine can count the rows, and a
 * prediction made here would be a claim about data this process has not read.
 * What the developer needs before deciding is that the two outcomes exist.
 */
export function demotePrompt(
  cluster: TrainingCluster,
  kind: string,
  name: string,
): TrainingPrompt {
  const detail = [
    `"${name}" stops being callable on ${cluster.label}. The withdrawal is persisted and every node applies it within seconds.`,
    "",
    "Only this construct. Anything it depends on stays promoted -- other constructs may still bind against it.",
    "",
    "If this is a concept: rows written under it outlive its definition, so one with rows is RETIRED rather than removed. It stays registered, its rows stay readable, new writes to it are refused, and its name stays claimed. Only a concept with no rows is removed outright.",
    "",
    "Demote is owner-only. If you are not the cluster owner the engine refuses and says so.",
  ].join("\n");
  return {
    message: `Demote ${kind} "${name}" from ${cluster.label}?`,
    detail,
    confirmLabel: "Demote",
  };
}

/**
 * breakingOverridePrompt: the SECOND act, after a promote was refused.
 *
 * Deliberately shaped so it cannot be reached by accident. It is not a checkbox
 * on the first modal (easy to leave ticked, and ticked before the diff exists),
 * it is not a retry of the same button, and the flag it unlocks is never carried
 * on a first attempt. What produced it is a refusal the engine already made,
 * carrying the classification below -- so the developer is overriding a specific
 * named change rather than a general objection.
 *
 * The button names the consequence, not the mechanism.
 */
export function breakingOverridePrompt(diffs: readonly ConceptSchemaDiff[]): TrainingPrompt {
  const breaking = diffs.filter((d) => d.breaking);
  const fields = breaking
    .flatMap((d) => d.changes.filter((c) => c.breaking))
    .map((c) => (c.field === "" ? c.kind : c.field));
  const detail = [
    "The engine refused this promote because the schema change would strand rows already written under the concept. Overriding lands it anyway.",
    "",
    conceptDiffReport(diffs),
    "",
    "The override is recorded. The engine audits it on v1:identity:auditEvent, naming the concept and the fields.",
  ].join("\n");
  return {
    message:
      breaking.length === 1
        ? `Promote ${breaking[0]!.concept} anyway, with a breaking schema change?`
        : `Promote ${breaking.length} concepts anyway, with breaking schema changes?`,
    detail,
    confirmLabel:
      fields.length === 1 ? `Override and break "${fields[0]}"` : "Override and promote",
  };
}

// -----------------------------------------------------------------------------
// The structured outcomes
// -----------------------------------------------------------------------------

/**
 * conceptDiffReport renders the engine's classification of a concept re-promote.
 *
 * FROM THE FIELDS, not from `summary`. The engine's rendered block is carried on
 * every diff and is appended verbatim underneath, so a reader gets the engine's
 * exact wording as well -- but the structure above it is built from `changes`,
 * which is what lets a change be read at a glance and what makes this testable
 * without pinning the engine's prose.
 */
export function conceptDiffReport(diffs: readonly ConceptSchemaDiff[]): string {
  if (diffs.length === 0) return "";
  const lines: string[] = [];
  for (const diff of diffs) {
    lines.push(
      `${diff.concept}  --  ${diff.breaking ? "BREAKING" : "additive"}${diff.overridden ? ", override applied" : ""}`,
    );
    for (const change of diff.changes) lines.push(...changeLines(change));
    if (diff.summary !== "") {
      lines.push("");
      lines.push("  The engine's own summary:");
      for (const line of diff.summary.split("\n")) lines.push(`    ${line}`);
    }
    lines.push("");
  }
  return lines.join("\n").trimEnd();
}

function changeLines(change: ConceptSchemaChange): string[] {
  // An EMPTY field is not a lost name: it means the change is about the concept
  // itself -- an edited description, a changed node type, a relationship added
  // or removed -- so the kind carries the whole story and there is nothing to
  // print beside it.
  const subject = change.field === "" ? change.kind : `${change.kind} ${change.field}`;
  const transition =
    change.was === "" && change.now === "" ? "" : `  ${describeSide(change.was)} -> ${describeSide(change.now)}`;
  const lines = [`  ${change.breaking ? "BREAKING" : "additive"}  ${subject}${transition}`];
  if (change.detail !== "") lines.push(`      ${change.detail}`);
  lines.push(`      ${rowsLine(change)}`);
  if (change.referencedBy.length > 0) {
    lines.push(`      referenced by ${change.referencedBy.join(", ")}`);
  }
  return lines;
}

/**
 * rowsLine is rule 3 of this module, in one function.
 *
 * `rowsAffected` is a real count taken against the live table -- when
 * `rowCountKnown` says one was taken. When it does not, the number is not a
 * small count, it is no count, and the honest rendering says which. Printing
 * "0 rows" for a node that never asked the database would read as reassurance.
 */
function rowsLine(change: ConceptSchemaChange): string {
  if (!change.rowCountKnown) {
    return "rows affected: not counted (this node has no database to count against)";
  }
  return `rows affected: ${change.rowsAffected}`;
}

function describeSide(value: string): string {
  return value === "" ? "(absent)" : value;
}

/**
 * demoteOutcomeReport renders what actually happened to each construct.
 *
 * The outcome is REPORTED rather than inferred, because both outcomes come back
 * ok=true and no reading of "it worked" establishes whether the name is
 * claimable again. An outcome string this build has never heard of is printed
 * verbatim with no claim attached -- the SDK deliberately does not rewrite it,
 * and neither does this.
 */
export function demoteOutcomeReport(outcomes: readonly DemoteOutcome[]): string {
  return outcomes.map(demoteOutcomeLine).join("\n");
}

function demoteOutcomeLine(outcome: DemoteOutcome): string {
  const subject =
    outcome.conceptId === ""
      ? `${outcome.kind} ${outcome.name}`
      : `${outcome.kind} ${outcome.name} (${outcome.conceptId})`;
  switch (outcome.outcome) {
    case DemoteOutcomeRetired:
      return `retired  ${subject}
    ${outcome.rowCount} row(s) exist under it, so it stays registered and its name stays claimed. The rows stay readable; new writes are refused. Re-promoting un-retires it.`;
    case DemoteOutcomeRemoved:
      return `removed  ${subject}
    Gone from the shared registry. The name is claimable again.`;
    default:
      return `${outcome.outcome}  ${subject}`;
  }
}

/**
 * promotedList / definedList render the identity lists the two successes carry.
 * Kept separate from the diff so a promote with no concept in it says something
 * useful rather than nothing at all.
 */
export function constructList(
  constructs: readonly { kind: string; name: string }[],
): string {
  return constructs.map((c) => `    ${c.kind} ${c.name}`).join("\n");
}
