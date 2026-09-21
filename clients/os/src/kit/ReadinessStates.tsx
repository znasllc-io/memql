import { useSession } from "../chrome/access";
import { useOsIfPresent } from "../chrome/state";
import type { Readiness } from "../live/readiness";
import { MODULE_NAMES, MODULE_SETTINGS_SECTION, type ModuleId } from "../system/modules";
import type { Verdict } from "../system/readinessFold";
import { sectionsFor } from "../system/registry";
import { Button, Panel, Subhead } from "./controls";
import { Caption } from "./Caption";
import { ProvenanceDot, type DotTone } from "./index";

// THE SETUP SURFACE AND THE SET UP GROUP (design record
// 2026-09-06-configuration-readiness, sections 5.3 to 5.5).
//
// # Not an error, and not styled as one
//
// An unconfigured app has nothing wrong with it -- nobody has told it what to
// talk to yet. So this is quiet chrome, like SurfaceRefused beside it: the
// `off` dot at display size as the mark, one headline, the module's own
// sentence from the env manifest, and the one act that resolves it. Red and
// amber are STATUS in this shell, and they appear on the Settings entry where
// the fix is, never across the body of an app that is merely waiting.
//
// # Three states that must not read the same
//
// "The feed has not loaded", "not set up" and "set up" all decide whether the
// app body renders. gateFor answers "unknown" for the first and the window
// frame draws NOTHING for it -- a configured cluster must never flash a setup
// screen for a frame, and a shell that does not yet know must show the app
// rather than a screen the person cannot dismiss.

export interface Gate {
  state: "unknown" | "ready" | "partial" | "unconfigured";
  /** Required modules that are not configured: the ones that gate. */
  unmet: ModuleId[];
  /** Wanted modules that are not configured: the ones that only mark. */
  wanted: ModuleId[];
}

const configured = (v: Verdict | null): boolean => v !== null && v.state === "configured";

/**
 * What one surface's requirements come to.
 *
 * `partial` rather than `unconfigured` when every unmet requirement is itself
 * partial: mid-rollout, one replica has the setting and another does not, and
 * throwing up a setup screen over a deploy in progress would tell somebody to
 * fix something that is already fixing itself.
 */
export function gateFor(
  readiness: Readiness | undefined,
  requires: readonly ModuleId[],
  wants: readonly ModuleId[],
): Gate {
  if (!readiness || !readiness.loaded) return { state: "unknown", unmet: [], wanted: [] };
  const unmet = requires.filter((id) => !configured(readiness.of(id)));
  const wanted = wants.filter((id) => !configured(readiness.of(id)));
  const anyPartial = requires.some((id) => readiness.of(id)?.state === "partial");
  if (unmet.some((id) => readiness.of(id)?.state !== "partial")) {
    return { state: "unconfigured", unmet, wanted };
  }
  if (anyPartial || wanted.length > 0) return { state: "partial", unmet, wanted };
  return { state: "ready", unmet, wanted };
}

/**
 * The mark's tone, or null for no mark at all.
 *
 * Null for both `unknown` and `ready`, which is the restraint that keeps this
 * from becoming furniture: a dot that sits on every app forever is chrome, and
 * this one draws only while a person is actually needed.
 */
export function markToneFor(gate: Gate): DotTone | null {
  if (gate.state === "unconfigured") return "needsSetup";
  if (gate.state === "partial") return "partlySetUp";
  return null;
}

/**
 * Sentence-case words for a verdict, the Set up group's vocabulary.
 *
 * `partial` HAS TWO CAUSES AND THEY GET TWO NAMES (memql#5259). Some nodes
 * set up and others not is a difference BETWEEN nodes, and "Partly set up"
 * sent the owner looking for a half-filled form that did not exist; it reads
 * "Set up on some nodes". A lane every node agrees is half-filled keeps
 * "Partly set up", which is what it is.
 *
 * `unreported` HAS TWO CAUSES TOO. Nobody answered is "Not reported"; nodes
 * answered and none of them could finish the check (a fleet read that failed,
 * say) is "Could not check" -- a different thing to go and look at, and
 * never a reason to send anybody to a form.
 *
 * What never gets a word here is a node the fold SET ASIDE as behind the
 * cluster: a verdict with stale nodes reads exactly as it would without them,
 * because they do not change it. The Cluster app names them.
 */
export function stateWords(v: Verdict | null): string {
  if (v === null) return "Not reported";
  if (v.state === "unreported") return v.unknown.length > 0 ? "Could not check" : "Not reported";
  if (v.state === "configured") return "Set up";
  if (v.state === "partial") return setUpOnSomeNodes(v) ? "Set up on some nodes" : "Partly set up";
  return "Not set up";
}

/** Whether a partial verdict is nodes that DIFFER, some of them set up. */
function setUpOnSomeNodes(v: Verdict): boolean {
  const configured = v.nodes.filter((n) => n.state === "configured").length;
  return configured > 0 && configured < v.nodes.length;
}

/**
 * The nodes behind a verdict, in one quiet line for the operator who hovers:
 * which nodes differ, which are catching up, which could not check. Empty when
 * there is nothing beyond the words -- every node agrees and every one voted.
 *
 * A TITLE, NOT A CAPTION. The Set up group and the Modules list are where a
 * person reads the answer; the nodes behind it are the Cluster app's Modules
 * detail, which lists every one with its time and reason. Printing the node
 * list into the row is how the raw `bff-b=unconfigured, bff-a=configured`
 * pairs came to sit beside every mark.
 */
export function verdictDetail(v: Verdict | null): string {
  if (v === null) return "";
  const parts: string[] = [];
  const notSetUp = v.nodes.filter((n) => n.state !== "configured").map((n) => n.nodeId);
  if (v.state === "partial" && notSetUp.length > 0 && notSetUp.length < v.nodes.length) {
    parts.push(`Not set up on ${nodeList(notSetUp)}.`);
  }
  if (v.stale.length > 0) {
    parts.push(`${countNodes(v.stale.length)} catching up: ${nodeList(v.stale)}. Not counted until each re-checks, which it does on its own.`);
  }
  if (v.unknown.length > 0) {
    parts.push(`${countNodes(v.unknown.length)} could not check: ${nodeList(v.unknown)}. Not counted; each retries on its own.`);
  }
  return parts.join(" ");
}

/**
 * The ONE quiet label a list row carries when the fold set nodes aside, or
 * "" when it set none aside -- or when the verdict's own words already say it
 * ("Could not check" needs no "2 could not check" beside it).
 *
 * One label, not two: a row's state cluster is a glance, and the nodes that
 * could not check outrank the ones catching up -- a failed read is something
 * an operator may need to act on, a node behind the cluster re-checks by
 * itself. Both are in the title and in the detail.
 */
export function setAsideLabel(v: Verdict | null): string {
  if (v === null || v.state === "unreported") return "";
  if (v.unknown.length > 0) return `${v.unknown.length} could not check`;
  if (v.stale.length > 0) return `${v.stale.length} catching up`;
  return "";
}

function countNodes(n: number): string {
  return n === 1 ? "1 node" : `${n} nodes`;
}

/** Up to three names, then how many more: a title is one line, not a table. */
function nodeList(ids: readonly string[]): string {
  if (ids.length <= 3) return ids.join(", ");
  return `${ids.slice(0, 3).join(", ")} and ${ids.length - 3} more`;
}

export function SurfaceUnconfigured({
  surface,
  unmet,
  descriptions,
  canSetUp,
  onSetUp,
}: {
  /** What they opened, named as they saw it: "Campaigns", or "Workbenches". */
  surface: string;
  unmet: readonly ModuleId[];
  /** The manifest descriptions, by module id; a missing one renders the module's name. */
  descriptions: Partial<Record<ModuleId, string>>;
  canSetUp: boolean;
  onSetUp: () => void;
}) {
  return (
    <div className="os-setup" data-os-setup-surface>
      {/* The `off` dot at display size: an empty ring, which reads as
          "nothing here yet" rather than "something is wrong". */}
      <span className="os-setup-mark" aria-hidden />
      <h2 className="os-setup-head">{surface} is not set up yet</h2>
      {unmet.map((id) => (
        <p key={id} className="os-setup-body">
          {descriptions[id] ?? `${MODULE_NAMES[id]} is needed first.`}
        </p>
      ))}
      {canSetUp ? (
        // "Set up <surface>" -- the same verb the group's rows use for the
        // finished state, so the word a person clicks is the word they see
        // afterwards.
        <Button tone="primary" onClick={onSetUp}>
          Set up {surface}
        </Button>
      ) : (
        <p className="os-caption os-setup-next">An owner or developer can set it up in Settings.</p>
      )}
    </div>
  );
}

/** Whether this actor may configure: owner or developer, as a SET (the Integrations gate). */
export function canConfigure(role: string): boolean {
  return role === "owner" || role === "developer";
}

/**
 * WHAT A MODULE OFFERS, as four answers over one set of facts.
 *
 * The decision was inline in `SetupGroup`'s row until the first-run wizard
 * needed the same one (epic memql#5106). The two surfaces draw it
 * differently -- the group as a cell in a three-column row, the wizard as
 * the body of a rail stop -- so what is shared is the DECISION and not the
 * chrome. A second copy of any section's role rule is a copy that drifts, and
 * the drift is silent: a button that navigates to a section `sectionsForRole`
 * does not return goes nowhere and says nothing.
 *
 * THE EXAMPLE THAT MOTIVATED THIS ALREADY MOVED, which is the argument for
 * the lookup rather than against it. It was "providers is owner-only, so a
 * developer gets words"; epic memql#5088's D7 widened that section to
 * owner-or-developer, and because both surfaces ask the registry, the
 * developer got the button with no edit to either of them. A restated rule
 * would have had two places to miss.
 */
export type ModuleAct =
  | { kind: "none" }
  | { kind: "deployment"; variables: string[] }
  | { kind: "open"; app: string; section: string; name: string }
  | { kind: "words"; place: string; name: string }
  /** The group is drawn ON the page that configures the module. */
  | { kind: "here"; name: string };

/** One page of one app: where a Set up group says it is drawn. */
export interface SetupPlace {
  app: string;
  section: string;
}

export function moduleActFor(args: {
  id: ModuleId;
  verdict: Verdict | null;
  /** The sections of an app THIS actor may reach (`useReach`). */
  sectionsOf: (appId: string) => readonly string[];
  /** Whether there is a window to open into at all. */
  canOpenWindows: boolean;
  /** Where the asking surface is drawn, when it is a page of an app. */
  here?: SetupPlace;
}): ModuleAct {
  const { id, verdict, sectionsOf, canOpenWindows, here } = args;
  // A module that is SET UP needs no act. Saying where it would be
  // configured, to somebody looking at a row that says "Set up", is an
  // instruction with nothing behind it -- and a column of them beside every
  // finished row is the furniture rule 10 exists to keep out.
  if (verdict?.state === "configured") return { kind: "none" };
  const target = MODULE_SETTINGS_SECTION[id];
  if (target === null) {
    // The lane that still needs something, else the first: what a person has
    // to set is the incomplete one. Only the ABSENT slots are named -- the
    // ones already set are not work.
    const lane = verdict?.lanes.find((l) => !l.complete) ?? verdict?.lanes[0];
    return {
      kind: "deployment",
      variables: (lane?.slots ?? []).filter((slot) => !slot.present).map((slot) => slot.name),
    };
  }
  // THREE things have to hold before a button is offered, and the third is
  // the one that is easy to miss.
  //
  //  - There is somewhere to send them (`target`).
  //  - There is a window to open. `canOpenWindows` is `!== "phone"` rather
  //    than `=== "desktop"`: the iPad chrome carries windows too.
  //  - THIS ACTOR MAY REACH THAT SECTION. Settings' own `providers` section
  //    is gated, so an actor offered "Open Doors" who cannot reach it would
  //    navigate a window to a section `sectionsForRole` does not return.
  //
  // Asked of the REGISTRY rather than restated here: a literal copy of
  // "providers is owner-only" would be a second place for that rule to live,
  // and the section's own manifest is the first.
  // ALREADY THERE. A module configured from the page this group is drawn on
  // -- Deployables' settings carry both the group and the Sources it points at
  // -- gets WORDS that say where to look. "Open Sources" on the page that holds
  // Sources is a button that goes nowhere, which is the thing the last arm of
  // this function exists to refuse.
  if (here !== undefined && here.app === target.app && here.section === target.section) {
    return { kind: "here", name: target.name };
  }
  const reachable = sectionsOf(target.app).includes(target.section);
  if (reachable && canOpenWindows) {
    return { kind: "open", app: target.app, section: target.section, name: target.name };
  }
  // No window to open into, or a section this actor cannot reach, so the
  // destination is named in words. A button that could not go anywhere would
  // be worse than a sentence that says where to look.
  return { kind: "words", place: target.place, name: target.name };
}

/**
 * The shell's reach, for a surface that does not know in advance WHICH app it
 * will hand off to: a module's home is data (`MODULE_SETTINGS_SECTION`), and a
 * hook cannot be called once per row of it. Null-shell-safe, like
 * `useAppReach` below and for its reason.
 */
export function useReach(): {
  sectionsOf: (appId: string) => string[];
  canOpenWindows: boolean;
  open: (appId: string, section: string, payload?: Record<string, unknown>) => void;
} {
  const os = useOsIfPresent();
  return {
    sectionsOf: (appId) => {
      const app = os?.registry.apps.find((a) => a.id === appId) ?? null;
      return app === null ? [] : sectionsFor(app).map((sec) => sec.id);
    },
    canOpenWindows: os !== null && os.layout !== "phone",
    open: (appId, section, payload) => {
      os?.actions.openApp(appId, section, payload);
    },
  };
}

/**
 * The sections of one app this actor may reach, whether there is a window to
 * open into, and how to open one.
 *
 * `useOsIfPresent` rather than `useOs`: a Set up group rendered in a test
 * with no shell around it is not a bug, and "there is nowhere to hand off
 * to" is exactly what null means. Every caller must therefore be able to say
 * its destination in WORDS as well as in a button, which is the discipline
 * this hook exists to force.
 */
export function useAppReach(appId: string): {
  sections: string[];
  canOpenWindows: boolean;
  open: (section: string, payload?: Record<string, unknown>) => void;
} {
  const os = useOsIfPresent();
  const app = os?.registry.apps.find((a) => a.id === appId) ?? null;
  return {
    sections: app === null ? [] : sectionsFor(app).map((sec) => sec.id),
    canOpenWindows: os !== null && os.layout !== "phone",
    open: (section, payload) => {
      os?.actions.openApp(appId, section, payload);
    },
  };
}

export function SetupGroup({
  app,
  requires,
  wants,
  readiness,
  here,
}: {
  app: string;
  requires: readonly ModuleId[];
  wants: readonly ModuleId[];
  /** Injected so the group is testable without a session feed; apps pass useSession().readiness. */
  readiness: Readiness | undefined;
  /** The page this group is drawn on, so a module configured ON it is pointed
   *  at in words rather than with a button that opens where they already are. */
  here?: SetupPlace;
}) {
  const { access } = useSession();
  const role = access?.role ?? "";
  const reach = useReach();
  if (!canConfigure(role)) return null;
  const ids = Array.from(new Set([...requires, ...wants]));
  if (ids.length === 0) return null;
  return (
    <Panel label={`Set up ${app}`}>
      <Subhead>Set up</Subhead>
      {!readiness || !readiness.loaded ? (
        <Caption>Reading this cluster&apos;s setup.</Caption>
      ) : (
        <div className="os-setup-group-rows">
          {ids.map((id) => {
            const v = readiness.of(id);
            const tone: DotTone | null =
              v === null || v.state === "unreported" || v.state === "configured"
                ? null
                : v.state === "partial"
                  ? "partlySetUp"
                  : "needsSetup";
            const act = moduleActFor({
              id,
              verdict: v,
              sectionsOf: reach.sectionsOf,
              canOpenWindows: reach.canOpenWindows,
              here,
            });
            return (
              <div key={id} className="os-setup-row">
                <span className="os-setup-name">{MODULE_NAMES[id]}</span>
                <span className="os-setup-state" title={verdictDetail(v) || undefined}>
                  {tone ? (
                    <ProvenanceDot tone={tone} label={`${MODULE_NAMES[id]}: ${stateWords(v)}`} />
                  ) : null}
                  {stateWords(v)}
                </span>
                {act.kind === "none" ? (
                  <span className="os-setup-act" />
                ) : act.kind === "deployment" ? (
                  <span className="os-setup-act">
                    <span className="os-caption">Set in the deployment</span>
                    {act.variables.length > 0 ? (
                      <p className="os-setup-vars">{act.variables.join(" ")}</p>
                    ) : null}
                  </span>
                ) : act.kind === "open" ? (
                  <span className="os-setup-act">
                    <Button onClick={() => reach.open(act.app, act.section)}>Open {act.name}</Button>
                  </span>
                ) : act.kind === "here" ? (
                  <span className="os-setup-act os-caption">Below, under {act.name}</span>
                ) : (
                  <span className="os-setup-act os-caption">{act.place}, under {act.name}</span>
                )}
              </div>
            );
          })}
        </div>
      )}
    </Panel>
  );
}
