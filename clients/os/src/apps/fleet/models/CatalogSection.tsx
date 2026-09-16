import { EmptyState, Button } from "../../../kit";
import { useMemo } from "react";

import { Caption, Fact, Facts, Notice, Subhead } from "../../../kit";
import {
  applyFacets,
  categorySentence,
  groupByCategory,
  hasUncheckableClass,
  joinCatalog,
  type CatalogFacets,
  type CatalogRow,
  type FleetMachineFacts,
  type FleetModelFacts,
  type ModelProfile,
} from "./catalog";
import type { CatalogModel } from "./useInference";

// The curated catalog, beside what this fleet actually serves
// (epic memql#5137, task memql#5140).
//
// ===========================================================================
// WHY THIS IS NOT ANOTHER CARD LIST
// ===========================================================================
// The ranked list above answers "what WILL be used" and is rendered as cards,
// because each entry is a thing with machines under it. This answers "what
// SHOULD you run, and what can you not" -- entries that are mostly one line,
// where the interesting ones carry a sentence. That is a definition-list
// shape, and giving it the card treatment would say the two lists are the same
// kind of thing when the whole reason this section exists is that they are not
// (rule 8 is one container LANGUAGE, not one container).
//
// It also spends no accent. The accent underline above is the one mark on this
// screen and it means "this is what your next turn lands on"; a second use here
// would divide the reader's attention between a fact about now and a
// recommendation about later. Served and unserved are told apart by INK, which
// is what the eye reads first anyway.
//
// ===========================================================================
// THE GAP IS THE CONTENT
// ===========================================================================
// A category the fleet fully serves collapses to one quiet line. A category it
// does not opens up and each entry says why. So the page is mostly empty where
// things are fine and dense where they are not, which is the shape of the
// question an operator brought.

/**
 * What the fleet's machines report, folded out of the model rows.
 *
 * EXPORTED FOR TEST. It is link three of memql#5195 -- the one that turns wire
 * fields into the facts the join compares -- and it was reachable only through
 * the component, so the whole of it was untested while it consisted of two
 * hardcoded defaults.
 */
export function machineFactsFrom(models: CatalogModel[]): FleetMachineFacts[] {
  const byId = new Map<string, FleetMachineFacts>();
  for (const model of models) {
    for (const m of model.machines) {
      const key = m.registrationId || m.name;
      if (!key) continue;
      const prior = byId.get(key);
      const runtimes = new Set([...(prior?.runtimes ?? []), ...m.runtimes]);
      byId.set(key, {
        name: m.displayName || m.name || key,
        runtimes: [...runtimes],
        online: m.online || (prior?.online ?? false),
        // ONE MACHINE APPEARS UNDER EVERY MODEL IT SERVES, so this is a fold and
        // not an assignment: the entries describe the same machine and carry the
        // same figures, and taking the first non-empty one keeps a fact that one
        // entry happened to omit. Before memql#5195 these two were the constants
        // "" and 0 -- nothing on the wire carried them, so the machine-class
        // floor was compared against a fleet size nobody had reported and was
        // never checked on any fleet.
        //
        // ABSENT STILL MEANS "HAS NOT SAID", which blocks nothing. A cockpit
        // that predates the hardware scanner sends no inventory, and guessing
        // would tell an operator their machine is too small when nobody has
        // asked it yet.
        platform: m.platform || (prior?.platform ?? ""),
        memoryGb: m.memoryGb || (prior?.memoryGb ?? 0),
      });
    }
  }
  return [...byId.values()];
}

function fleetFactsFrom(models: CatalogModel[]): FleetModelFacts[] {
  return models.map((m) => ({
    modelId: m.modelId,
    online: m.online,
    machineCount: m.machineCount,
  }));
}

export function CatalogSection({
  onClearFilters,
  profiles,
  profilesState,
  profilesError,
  fleet,
  facets,
}: {
  onClearFilters?: () => void;
  profiles: ModelProfile[];
  profilesState: string;
  profilesError: string;
  fleet: CatalogModel[];
  /**
   * Optional narrowing, from a Refine control on the Models Head.
   *
   * IT IS A PROP RATHER THAN STATE HERE because the control belongs on the
   * HEAD (DESIGN.md rule 2: filters live behind one affordance on the Head
   * line, not as chrome over the content) and this section is not the Head.
   * Epic memql#5153 owns the control; this owns what it narrows.
   */
  facets?: CatalogFacets;
}) {
  // Folded ONCE. Two memos each calling machineFactsFrom built the same map
  // twice per render, and more to the point they could disagree: the join and
  // the count are two readings of one fleet, and a surface that asks the same
  // question twice is a surface where the two answers can drift.
  const machines = useMemo(() => machineFactsFrom(fleet), [fleet]);
  const reading = useMemo(
    () => joinCatalog(profiles, fleetFactsFrom(fleet), machines),
    [profiles, fleet, machines],
  );
  const machineCount = machines.length;
  const groups = useMemo(
    () => applyFacets(groupByCategory(reading, machineCount > 0), facets ?? {}),
    [reading, machineCount, facets],
  );

  if (profilesState === "failed") {
    return (
      <>
        <Subhead>What to run</Subhead>
        <Notice
          tone="info"
          sentence="We could not read the model catalog."
          next="Refresh to try reading the catalog again."
          detail={profilesError}
        />
      </>
    );
  }

  // NOTHING IS SHOWN OVER AN EMPTY CATALOG, the same rule the ranked list
  // follows: every sentence below describes a set of recommendations, and
  // printing them above none describes nothing.
  if (groups.length === 0) return profilesState === "reading" ? <Caption>Reading the model catalog…</Caption> : <EmptyState title={profiles.length ? "No matching models" : "Model catalog unavailable"} action={profiles.length && onClearFilters ? <Button onClick={onClearFilters}>Clear filters</Button> : undefined}>{profiles.length ? "Choose another category or runtime to see more models." : "This cluster has not provided a model catalog. You can still use models already installed on your machines."}</EmptyState>;

  return (
    <>
      <Subhead>What to run</Subhead>
      <Caption>
        Explore models by capability and check which fit your machines. Models you already serve remain available even if they are not in this catalog.
      </Caption>

      {machineCount > 0 ? null : (
        // SAID ONCE, here, rather than on every category (rule 7). With no
        // machine paired, "pair one" is the same answer for all nine groups and
        // the only thing this person can do next -- so it is one sentence above
        // the list, and the groups below it fall silent.
        <Caption>
          Connect a machine from Machines to check compatibility and install models.
        </Caption>
      )}

      {machineCount > 0 && hasUncheckableClass(groups) ? (
        // The same rule, for the same reason: it is one fact about the FLEET --
        // the floor cannot be checked for any entry that has one, under every
        // category at once -- so it is said once above the list rather than
        // repeated under each group (rule 7).
        //
        // SINCE memql#5195 THIS IS A REAL STATE RATHER THAN THE ONLY STATE. The
        // machine entries now carry memory, so this reads for a cockpit that
        // predates the hardware scanner and for nothing else; a fleet that
        // reported and is simply too small says so on the entries themselves,
        // which is a different sentence and the correct one.
        //
        // Entries stay UNBLOCKED here: guessing would tell somebody their machine
        // is too small when nobody has asked it yet. What the list does not do is
        // count an unchecked entry as one that "runs on a machine you already
        // have", which was a claim about their hardware made out of the absence
        // of data about it.
        <Caption>
          Your machines have not reported their memory yet, so this list cannot say which of these
          they can run.
        </Caption>
      ) : null}

      <div className="os-fleet-catalog">
        {groups.map((group) => {
          const state = categorySentence(group);
          return (
          <details className="os-fleet-catgroup" key={group.category} open={groups.length === 1}>
            <summary className="os-fleet-catname">{group.label} <span className="os-caption">{group.shown.length} models</span></summary>
            {state === "" ? null : <p className="os-fleet-catstate">{state}</p>}
            <ul className="os-fleet-catrows">
              {group.shown.map((row) => (
                <CatalogEntry key={`${row.profile.category}:${row.profile.modelId}`} row={row} />
              ))}
            </ul>
          </details>
          );
        })}
      </div>

      {reading.uncatalogued.length === 0 ? null : (
        <>
          <Caption>
            Your fleet also serves {reading.uncatalogued.length}{" "}
            {reading.uncatalogued.length === 1 ? "model" : "models"} the catalog does not list. They
            work exactly as any other — the catalog is what we recommend, not what is allowed.
          </Caption>
          <ul className="os-fleet-catrows">
            {reading.uncatalogued.map((m) => (
              <li className="os-fleet-catrow" key={m.modelId} data-served>
                <span className="os-fleet-catid os-mono">{m.modelId}</span>
                <span className="os-fleet-catnote">
                  on {m.machineCount} {m.machineCount === 1 ? "machine" : "machines"}
                  {m.online ? "" : ", none awake"}
                </span>
              </li>
            ))}
          </ul>
        </>
      )}
    </>
  );
}

function CatalogEntry({ row }: { row: CatalogRow }) {
  const { profile, served, blocked } = row;

  // The right-hand text does ONE job per state, and the three are different
  // jobs: confirm, explain, or invite. A single field carrying all three would
  // make the reader work out which they were looking at.
  const note = served
    ? profile.notes
    : blocked
      ? blocked.detail
      : joinSize(sizeWords(profile.sizeBytes), profile.notes);

  return (
    <li
      className="os-fleet-catrow"
      data-served={served || undefined}
      data-blocked={blocked ? blocked.kind : undefined}
    >
      <details className="fleet-catalog-profile"><summary><span className="os-fleet-catid os-mono">{profile.modelId}</span><span className="os-fleet-catnote">{served ? "Installed" : blocked ? blocked.detail : "Available to install"}</span></summary>
        <p className="os-caption">{note}</p>
        <Facts><Fact label="Runtime" value={profile.runtime} /><Fact label="Family" value={profile.family || "Not reported"} /><Fact label="Parameters" value={profile.params || "Not reported"} /><Fact label="Context" value={profile.contextWindow || "Not reported"} /><Fact label="Quantization" value={profile.quant || "Not reported"} /><Fact label="Capabilities" value={profile.flags.join(", ") || "None reported"} /><Fact label="Dimensions" value={profile.dimensions || "Not applicable"} /><Fact label="License" value={profile.license || "Not reported"} /><Fact label="Recommended for" value={profile.recommendedFor.join(", ") || "Not specified"} /><Fact label="Machine class" value={profile.minMachineClass || "Not specified"} /><Fact label="Platforms" value={profile.offeredOn.join(", ") || "Not specified"} /></Facts>
      </details>
    </li>
  );
}

/**
 * Size and note, joined only when there is a size.
 *
 * The em dash is a SEPARATOR, so it appears only when it has two things to
 * separate. Interpolating it unconditionally left every entry whose publisher
 * states no download size reading "— The fast image answer", which looks like a
 * missing value and is in fact a value that was never claimed.
 */
function joinSize(size: string, notes: string): string {
  if (size === "") return notes;
  if (notes === "") return size;
  return `${size} — ${notes}`;
}

/**
 * A download size in the words an operator uses, or the empty string when the
 * publisher did not state one.
 *
 * ZERO IS NOT "0 GB". The catalog leaves sizeBytes at zero for every entry
 * whose publisher does not state a download size, and printing that as a
 * measurement would be an invented number that reads exactly like a real one.
 */
function sizeWords(bytes: number): string {
  if (bytes <= 0) return "";
  const gb = bytes / 1_000_000_000;
  if (gb >= 10) return `${Math.round(gb)} GB`;
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  return `${Math.round(bytes / 1_000_000)} MB`;
}
