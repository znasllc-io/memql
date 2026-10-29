import { useCallback, useEffect, useRef, useState } from "react";
import { Concepts, isTransportInterruption, rowString, type QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, ContentSkeleton, Details, DetailsSection, Notice, Switch, flatten } from "../../kit";
import { PageHelp } from "../../kit/pageHelp";
import { useOsConnection } from "../../live/connection";
import { useGraphRevision } from "../../live/useGraphRevision";
import { useReadEffect } from "../../live/useReadEffect";
import { usePaneActive } from "../deployables/paneActivity";

// Pipelines > Settings > AI (owner decision, 2026-10-29): the two pipeline
// features that ask a model, one switch each, both OFF unless turned on here.
//
// ===========================================================================
// ONE GLOBAL VARIABLE, READ THE WAY THE ENGINE READS IT
// ===========================================================================
// `MEMQL_PIPELINES_AI` holds `{"review": bool, "diagnosis": bool}` and the
// engine reads it with `pipelines.ParseAISettings` (component/pipelines/ai.go):
// an absent row, an empty or unparseable value, a non-boolean, or a key it does
// not know reads as BOTH OFF, so a typo can never switch a model on. This
// parses the same way, keys compared case-insensitively like Go's decoder,
// because a switch drawn "on" over a value the engine reads as off (or the
// reverse) would misstate what the next run does.
//
// ===========================================================================
// A SAVE WRITES THE WHOLE OBJECT, UNDER THE ROW'S OWN ID
// ===========================================================================
// Each toggle writes both keys through `setGlobalVariable`, so the stored value
// is always one this page can read back. The id is the existing row's, or the
// seeder's derivation (`var-global-<slug>`) when there is none yet: the
// resolver looks rows up by NAME, and a second id would add a second row with
// the same name rather than update the first.
//
// ===========================================================================
// THE SWITCHES SHOW WHAT THE CLUSTER HOLDS
// ===========================================================================
// No local draft, as on a machine's owner-only features: every save is
// followed by a read, and only that read moves a switch. A refused write, or a
// write the read-back does not confirm, leaves the switch where the cluster has
// it, with the failure beside it and a retry while nothing changed meanwhile.
// Switches are disabled while a write or a read is in flight, so a press is
// always computed from a value the cluster has just answered.
//
// Reads follow the concept's graph events, reconnection and app resume
// (`useGraphRevision`), and stop while the pane is hidden. There is no role
// gate here: who may write is the engine's decision, and a refusal is shown in
// the engine's own words.
//
// Regression tests: test/pipelines/aiSettings.test.tsx.

/** The global variable holding the switches (`pipelines.AISettingsVariable`). */
const PIPELINES_AI_VARIABLE = "MEMQL_PIPELINES_AI";
/** The seeder's row id for that variable, used only while no row exists. */
const PIPELINES_AI_ROW_ID = "var-global-memql-pipelines-ai";
const PIPELINES_AI_DESCRIPTION =
  "Pipeline AI features: independent pull-request review and failed-run diagnosis. Off unless turned on here.";

export interface PipelinesAI {
  /** A model reviews a pull request after its checks pass; a rejection fails the check. */
  review: boolean;
  /** A model diagnoses a run's first failure and may propose a repair for approval. */
  diagnosis: boolean;
}

export const PIPELINES_AI_OFF: PipelinesAI = { review: false, diagnosis: false };

const KEYS = ["review", "diagnosis"] as const;

/** The switches a stored value turns on, read as `pipelines.ParseAISettings` reads it. */
export function parsePipelinesAI(raw: string): PipelinesAI {
  if (raw.trim() === "") return PIPELINES_AI_OFF;
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return PIPELINES_AI_OFF;
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) return PIPELINES_AI_OFF;
  const out = { ...PIPELINES_AI_OFF };
  for (const [key, field] of Object.entries(value)) {
    const name = KEYS.find((known) => known === key.toLowerCase());
    if (name === undefined) return PIPELINES_AI_OFF;
    // JSON null leaves a Go bool unset; anything else that is not a boolean
    // fails the engine's decode, which reads as all off.
    if (field === null) continue;
    if (typeof field !== "boolean") return PIPELINES_AI_OFF;
    out[name] = field;
  }
  return out;
}

/** The value a save writes: both keys, always. */
function serializePipelinesAI(settings: PipelinesAI): string {
  return JSON.stringify({ review: settings.review, diagnosis: settings.diagnosis });
}

function sameAI(a: PipelinesAI, b: PipelinesAI): boolean {
  return a.review === b.review && a.diagnosis === b.diagnosis;
}

interface StoredAI {
  /** The row a save updates: the stored row's id, or the seeder's. */
  id: string;
  settings: PipelinesAI;
}

async function readPipelinesAI(query: QueryClient, signal?: AbortSignal): Promise<StoredAI> {
  const result = await query.globalVariable({ name: PIPELINES_AI_VARIABLE }, signal ? { signal } : undefined);
  const raw = result.rows()[0];
  if (raw === undefined) return { id: PIPELINES_AI_ROW_ID, settings: PIPELINES_AI_OFF };
  const row = flatten(raw);
  return { id: rowString(row, "id") || PIPELINES_AI_ROW_ID, settings: parsePipelinesAI(rowString(row, "value")) };
}

async function writePipelinesAI(query: QueryClient, id: string, settings: PipelinesAI): Promise<void> {
  await query.setGlobalVariable({
    id,
    name: PIPELINES_AI_VARIABLE,
    value: serializePipelinesAI(settings),
    description: PIPELINES_AI_DESCRIPTION,
    active: true,
  });
}

const messageOf = (err: unknown) => (err instanceof Error ? err.message : String(err));

interface AIRead {
  /** The newest answer that landed; null before the first. */
  stored: StoredAI | null;
  /** True only while `stored` answers the newest read. */
  fresh: boolean;
  /** The newest read's refusal, verbatim. Empty when it answered, or was only
   *  interrupted (the shell reports the connection; recovery reads again). */
  error: string;
}

function usePipelinesAI(query: QueryClient | null, active: boolean) {
  const revision = useGraphRevision([Concepts.PLATFORM_GLOBAL_VARIABLE], active);
  const [read, setRead] = useState<AIRead>({ stored: null, fresh: false, error: "" });
  // The newest read wins: an older one landing late never replaces its answer.
  const newest = useRef(0);
  const load = useCallback(async (signal?: AbortSignal): Promise<StoredAI | null> => {
    if (query === null) return null;
    const mine = ++newest.current;
    setRead((held) => ({ ...held, fresh: false }));
    try {
      const stored = await readPipelinesAI(query, signal);
      if (!signal?.aborted && mine === newest.current) setRead({ stored, fresh: true, error: "" });
      return stored;
    } catch (err) {
      if (!signal?.aborted && mine === newest.current) {
        setRead((held) => ({ stored: held.stored, fresh: false, error: isTransportInterruption(err) ? "" : messageOf(err) }));
      }
      return null;
    }
  }, [query]);
  useReadEffect(async (signal) => { await load(signal); }, [load], revision);
  return { read, load };
}

interface SaveFailure {
  choice: PipelinesAI;
  /** What the cluster held when the save began, to tell a retry from a race. */
  base: PipelinesAI;
  /** The write's outcome is unknown: the connection dropped while it was sent. */
  interrupted: boolean;
  /** The engine's own words; empty when the read-back is what disagreed. */
  detail: string;
}

export function PipelinesAISettings() {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const { read, load } = usePipelinesAI(query, usePaneActive());
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState<SaveFailure | null>(null);
  const inFlight = useRef(false);
  const region = useRef<HTMLElement>(null);
  const returnFocus = useRef<keyof PipelinesAI | null>(null);
  const stored = read.stored;
  const current = stored?.settings ?? null;
  const locked = saving || !read.fresh || query === null;

  // A switch disabled while it holds focus drops it to the page, so a keyboard
  // user would lose their place on every toggle. Once the switches take presses
  // again, focus returns to the one that was changed, unless the person has
  // since put it somewhere else.
  useEffect(() => {
    const key = returnFocus.current;
    if (locked || key === null) return;
    returnFocus.current = null;
    const active = document.activeElement;
    if (active !== null && active !== document.body) return;
    region.current?.querySelector<HTMLInputElement>(`[data-ai-setting="${key}"] input`)?.focus();
  }, [locked]);

  async function save(choice: PipelinesAI, key: keyof PipelinesAI) {
    if (inFlight.current || locked || stored === null || query === null) return;
    inFlight.current = true;
    returnFocus.current = key;
    setSaving(true);
    setFailure(null);
    let failed: SaveFailure | null = null;
    try {
      await writePipelinesAI(query, stored.id, choice);
    } catch (err) {
      const interrupted = isTransportInterruption(err);
      failed = { choice, base: stored.settings, interrupted, detail: interrupted ? "" : messageOf(err) };
    }
    // Read back either way: a switch moves only on the cluster's answer. A
    // read that does not land says so itself and claims nothing about the save.
    const after = await load();
    if (failed === null && after !== null && !sameAI(after.settings, choice)) {
      failed = { choice, base: stored.settings, interrupted: false, detail: "" };
    }
    setFailure(failed);
    setSaving(false);
    inFlight.current = false;
  }

  // A failure the cluster has since caught up with (a dropped write that did
  // land, the same choice made elsewhere) has nothing left to report.
  const standing = failure !== null && current !== null && !sameAI(current, failure.choice) ? failure : null;
  const readFailure = read.error === "" ? null : (
    <Notice tone="warn" sentence="AI settings could not be read." detail={read.error}>
      <Button disabled={saving} onClick={() => void load()}>Try again</Button>
    </Notice>
  );

  return (
    <section ref={region} className="os-settings os-settings-wide deployable-settings" aria-label="AI settings" aria-busy={saving || undefined}>
      <PageHelp title="AI review and diagnosis">
        <p>
          A switch allows its feature; each pipeline's installed policy still decides whether it is used. With review on, a
          pipeline whose policy names no reviewer is still decided by its own checks. With diagnosis on, the maintenance
          policy still decides whether a failure gets an analysis, a person or nothing.
        </p>
      </PageHelp>
      {current === null ? (
        (readFailure ?? <ContentSkeleton kind="detail" label="Loading AI settings" />)
      ) : (
        <Details>
          <DetailsSection title="All pipelines">
            <div className="os-app-stack">
              <div data-ai-setting="review">
                <Switch checked={current.review} disabled={locked} onChange={(review) => void save({ ...current, review }, "review")}>
                  AI review of pull requests
                </Switch>
                <Caption>
                  When on, a model reviews each pull request after its checks pass, and a rejection fails the check. When
                  off, the pipeline decides on its own checks.
                </Caption>
              </div>
              <div data-ai-setting="diagnosis">
                <Switch checked={current.diagnosis} disabled={locked} onChange={(diagnosis) => void save({ ...current, diagnosis }, "diagnosis")}>
                  AI diagnosis of failed runs
                </Switch>
                <Caption>
                  When on, a model diagnoses a run's first failure and can propose a repair for the owner to approve. When
                  off, a failed run just reports its failure.
                </Caption>
              </div>
              {standing ? (
                <Notice
                  tone="error"
                  sentence={standing.interrupted ? "The change could not be confirmed." : "The change was not saved."}
                  detail={standing.detail || undefined}
                >
                  {sameAI(current, standing.base) ? (
                    <Button disabled={locked} onClick={() => void save(standing.choice, KEYS.find((key) => standing.choice[key] !== standing.base[key]) ?? "review")}>
                      Try again
                    </Button>
                  ) : (
                    <Caption>Settings changed elsewhere. Review the current choices and try again.</Caption>
                  )}
                </Notice>
              ) : null}
              {readFailure}
              <Caption>Changes apply to the next review or failure; nothing already running changes.</Caption>
            </div>
          </DetailsSection>
        </Details>
      )}
    </section>
  );
}
