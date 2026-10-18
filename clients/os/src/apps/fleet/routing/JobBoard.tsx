import { useEffect, useId, useRef, useState, type DragEvent } from "react";
import { BrainCircuit, GripVertical, LockKeyhole, MessageSquare, Plus, Search, X, Zap } from "lucide-react";

import "../machines/machineModels.css";
import { Subhead } from "../../../kit";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { IconButton } from "../../../kit/IconButton";
import { RUNNABLE_APPS } from "../rows";
import { EDITABLE_JOBS, JOB_MEANING, JOB_TITLES, MAX_JOB_SOURCES, type EditableJob, type EmbedderReading, type JobId, type JobSources } from "./routeModes";
import { ReadinessDot, SourceGlyph } from "./SourceGlyph";
import { readSource, servingIndex, type RoutingFacts, type SourceReading } from "./sources";
import { sourceKind, sourceLabel } from "./vocabulary";

// A route's jobs beside the sources that can serve them -- the machine Model
// setup board's grammar (DESIGN.md, "Machine Model setup uses an assignment
// board"), applied to a route.
//
//   assign    select a source, then Add on a job; or drag the source onto it.
//             The selection stays, so one source can go to several jobs.
//   reorder   a job's sources are tried in order: drag one, or focus it and
//             use the arrow keys.
//   remove    its x, or Delete / Backspace on it.
//
// A JOB THAT CANNOT TAKE THE SELECTED SOURCE SAYS WHY, and offers nothing: a
// duplicate, a full job, a model that only makes embeddings. The same check
// refuses a drop, in the same words, on the job it was aimed at.
//
// SEARCH AND MEMORY IS NOT A TARGET. Every route serves embeddings from the
// cluster's bound model (one index, one vector space), so the row names that
// model, carries a lock and takes nothing.
//
// READINESS IS READ, NEVER SIMULATED (sources.ts): a job says "Serves with X"
// only when X can take a call right now -- a machine online with a free slot
// and the memory to load the model, an app signed in, a vendor set up.

const JOB_ICONS: Record<JobId, typeof Zap> = { fast: Zap, strong: MessageSquare, reasoning: BrainCircuit, embeddings: Search };

type Drag = { kind: "tray"; entry: string } | { kind: "chip"; job: EditableJob; from: number } | null;

/**
 * A reason in sentence case: readings carry them lower-case, for joining. A
 * first word that is a name -- a model id, a machine -- keeps its own case.
 */
export function sentence(text: string): string {
  const first = text.split(/\s/, 1)[0] ?? "";
  return /^[a-z]+[,.:;]?$/.test(first) ? text.charAt(0).toUpperCase() + text.slice(1) : text;
}

/** What a source says under its name: where it serves from, or why it cannot. */
export function sourceLine(reading: SourceReading): string {
  if (reading.readiness === "ready") return reading.fact ? sentence(reading.fact) : "Ready";
  if (reading.readiness === "unknown") return "Not checked";
  return sentence(reading.reason) || "Not ready";
}

/**
 * Why this source cannot go into this job, or "".
 *
 * `at` is the position being replaced by a reorder: a source already in that
 * position is not a duplicate of itself.
 */
export function jobPlacementProblem(entry: string, list: readonly string[], facts: RoutingFacts, at?: number): string {
  const others = list.filter((_, i) => i !== at);
  if (others.includes(entry)) return "Already in this job";
  if (at === undefined && list.length >= MAX_JOB_SOURCES) return `A job holds ${MAX_JOB_SOURCES} sources at most`;
  const kind = sourceKind(entry);
  if (kind !== "local" && kind !== "app" && kind !== "vendor") return "Not a source a route can use";
  if (kind === "local" && readSource(entry, facts).embeddingsOnly) return "Only makes embeddings";
  return "";
}

/** A job's state in a few words: what serves it now, or why nothing does. */
export function jobStatus(list: readonly string[], facts: RoutingFacts): { text: string; ready: boolean } {
  if (list.length === 0) return { text: "No source yet", ready: false };
  const at = servingIndex(list, facts);
  if (at.index >= 0) return { text: `Serves with ${readSource(list[at.index]!, facts).serves}`, ready: true };
  if (at.blockedAt >= 0) return { text: facts.machinesRead ? "Not checked" : "", ready: false };
  return { text: sentence(readSource(list[0]!, facts).reason) || "Nothing ready", ready: false };
}

/** The tray: every source a route can name, grouped the way a person looks for one. */
export function trayFor(facts: RoutingFacts): { id: string; title: string; sources: SourceReading[] }[] {
  const chatModels = Array.from(
    new Set(facts.machines.flatMap((m) => m.models.filter((model) => !model.embeddings).map((model) => model.modelId))),
  ).sort();
  return [
    { id: "models", title: "Models", sources: ["fleet:strongest", "fleet:fastest", ...chatModels.map((id) => `fleet:${id}`)].map((e) => readSource(e, facts)) },
    { id: "apps", title: "Apps", sources: ["app:*", ...RUNNABLE_APPS.map((id) => `app:${id}`)].map((e) => readSource(e, facts)) },
    { id: "vendors", title: "Vendors", sources: ["federation:cheapest", "federation:strongest"].map((e) => readSource(e, facts)) },
  ];
}

export function JobBoard({
  jobs,
  onChange,
  facts,
  embedder,
  readOnly = false,
  onAnnounce,
  onAddMachine,
  onOpenModels,
}: {
  jobs: JobSources;
  onChange: (next: JobSources) => void;
  facts: RoutingFacts;
  /** The cluster's bound embedding model, as read. */
  embedder: EmbedderReading;
  readOnly?: boolean;
  onAnnounce?: (sentence: string) => void;
  onAddMachine?: () => void;
  /** Opens Fleet's Models, where an embedding model is pulled and bound. */
  onOpenModels?: () => void;
}) {
  const hint = useId();
  const [selected, setSelected] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<{ job: EditableJob; message: string } | null>(null);
  const [dropJob, setDropJob] = useState<EditableJob | null>(null);
  const drag = useRef<Drag>(null);
  const chipRefs = useRef(new Map<string, HTMLButtonElement>());
  const [focusChip, setFocusChip] = useState<string | null>(null);

  // FOCUS FOLLOWS THE SOURCE IT WAS ON: a reorder makes a new element in a new
  // place, and a person who pressed an arrow expects to press it again.
  useEffect(() => {
    if (focusChip === null) return;
    chipRefs.current.get(focusChip)?.focus();
    setFocusChip(null);
  }, [focusChip, jobs]);

  const groups = readOnly ? [] : trayFor(facts);
  const noMachines = facts.machinesRead && facts.machines.length === 0;

  function place(job: EditableJob, entry: string, at = jobs[job].length) {
    const problem = jobPlacementProblem(entry, jobs[job], facts);
    if (problem !== "") {
      setRefusal({ job, message: problem });
      onAnnounce?.(`${JOB_TITLES[job]}: ${problem}.`);
      return;
    }
    setRefusal(null);
    const next = [...jobs[job]];
    next.splice(Math.min(at, next.length), 0, entry);
    onChange({ ...jobs, [job]: next });
    onAnnounce?.(`${sourceLabel(entry)} added to ${JOB_TITLES[job]}, position ${Math.min(at, jobs[job].length) + 1}.`);
  }

  function move(job: EditableJob, from: number, to: number) {
    const list = jobs[job];
    if (to < 0 || to >= list.length || from === to) return;
    const next = [...list];
    const [held] = next.splice(from, 1);
    next.splice(to, 0, held!);
    setRefusal(null);
    onChange({ ...jobs, [job]: next });
    setFocusChip(`${job}:${to}`);
    onAnnounce?.(`${sourceLabel(held!)} moved to position ${to + 1} in ${JOB_TITLES[job]}.`);
  }

  function remove(job: EditableJob, index: number) {
    const list = jobs[job];
    const label = sourceLabel(list[index]!);
    setRefusal(null);
    onChange({ ...jobs, [job]: list.filter((_, i) => i !== index) });
    if (list.length > 1) setFocusChip(`${job}:${Math.min(index, list.length - 2)}`);
    onAnnounce?.(`${label} removed from ${JOB_TITLES[job]}.`);
  }

  function dropOn(job: EditableJob, at: number | undefined, event: DragEvent) {
    event.preventDefault();
    event.stopPropagation();
    const held = drag.current;
    drag.current = null;
    setDropJob(null);
    if (held === null) return;
    if (held.kind === "chip") {
      if (held.job === job) move(job, held.from, Math.min(at ?? jobs[job].length - 1, jobs[job].length - 1));
      return;
    }
    place(job, held.entry, at);
  }

  const allowDrop = (job: EditableJob) => (event: DragEvent) => {
    if (readOnly || drag.current === null) return;
    const held = drag.current;
    if (held.kind === "chip" && held.job !== job) return;
    event.preventDefault();
    if (held.kind === "tray") setDropJob(job);
  };

  return (
    <div className="fleet-assignment-editor fleet-route-board-frame" aria-label="Jobs and their sources">
      <p id={hint} className="os-sr-only">
        A job tries its sources in order. Arrow keys move a source, Delete removes it.
      </p>
      <div className="fleet-assignment-board fleet-route-board" data-read-only={readOnly || undefined}>
        {readOnly ? null : (
          <section aria-label="Sources" className="fleet-route-tray">
            <Subhead>Sources</Subhead>
            {groups.map((group) => (
              <div key={group.id} className="fleet-route-tray-group" role="group" aria-label={group.title}>
                <span className="fleet-route-tray-title">{group.title}</span>
                <div className="fleet-assignment-models">
                  {group.sources.map((reading) => (
                    <button
                      key={reading.entry}
                      type="button"
                      className="fleet-assignment-model fleet-route-source"
                      data-ready={reading.readiness === "ready" || undefined}
                      aria-pressed={selected === reading.entry}
                      aria-label={`${reading.label}: ${sourceLine(reading)}`}
                      draggable
                      onClick={() => {
                        setRefusal(null);
                        setSelected((held) => (held === reading.entry ? null : reading.entry));
                      }}
                      onKeyDown={(event) => {
                        if (event.key === "Escape") setSelected(null);
                      }}
                      onDragStart={(event) => {
                        drag.current = { kind: "tray", entry: reading.entry };
                        setSelected(reading.entry);
                        event.dataTransfer?.setData("text/plain", reading.entry);
                      }}
                      onDragEnd={() => {
                        drag.current = null;
                        setDropJob(null);
                      }}
                    >
                      <GripVertical size={13} aria-hidden className="fleet-assignment-grip" />
                      <span className="fleet-route-source-glyph"><SourceGlyph kind={reading.kind} size={15} /></span>
                      <span className="fleet-assignment-model-body">
                        <strong>{reading.label}</strong>
                        <span className="fleet-assignment-model-tasks">
                          <ReadinessDot readiness={reading.readiness} /> {sourceLine(reading)}
                        </span>
                      </span>
                    </button>
                  ))}
                  {group.id === "models" && noMachines && onAddMachine ? (
                    <button type="button" className="os-link fleet-route-tray-act" onClick={onAddMachine}>Add a machine</button>
                  ) : null}
                </div>
              </div>
            ))}
          </section>
        )}

        <section aria-label="Jobs" className="fleet-route-jobs">
          {readOnly ? null : <Subhead>Jobs</Subhead>}
          <div className="fleet-route-job-grid">
            {EDITABLE_JOBS.map((job) => {
              const Icon = JOB_ICONS[job];
              const list = jobs[job];
              const status = jobStatus(list, facts);
              const problem = selected === null ? "" : jobPlacementProblem(selected, list, facts);
              return (
                <div
                  key={job}
                  className="fleet-route-job"
                  role="group"
                  aria-label={JOB_TITLES[job]}
                  data-drop-target={dropJob === job || undefined}
                  data-refused={refusal?.job === job || undefined}
                  onDragOver={allowDrop(job)}
                  onDragLeave={() => setDropJob((held) => (held === job ? null : held))}
                  onDrop={(event) => dropOn(job, undefined, event)}
                >
                  <div className="fleet-assignment-task-name fleet-route-job-name">
                    <Icon size={16} aria-hidden />
                    <span>{JOB_TITLES[job]}</span>
                  </div>
                  <p className="fleet-route-job-meaning">{JOB_MEANING[job]}</p>
                  {list.length === 0 ? (
                    <p className="fleet-route-job-empty">No source yet</p>
                  ) : (
                    <ol className="fleet-route-job-sources" aria-label={`${JOB_TITLES[job]} sources, in the order they are tried`}>
                      {list.map((entry, i) => {
                        const reading = readSource(entry, facts);
                        const key = `${job}:${i}`;
                        return (
                          <li
                            key={`${i}:${entry}`}
                            className="fleet-route-chip"
                            data-ready={reading.readiness === "ready" || undefined}
                            onDragOver={allowDrop(job)}
                            onDrop={(event) => dropOn(job, i, event)}
                          >
                            <button
                              ref={(el) => {
                                if (el) chipRefs.current.set(key, el);
                                else chipRefs.current.delete(key);
                              }}
                              type="button"
                              className="fleet-route-chip-body"
                              draggable={!readOnly}
                              tabIndex={readOnly ? -1 : 0}
                              aria-describedby={readOnly ? undefined : hint}
                              aria-keyshortcuts={readOnly ? undefined : "ArrowUp ArrowDown Delete"}
                              aria-label={`${i + 1}. ${reading.label}, ${sourceLine(reading)}`}
                              onKeyDown={(event) => {
                                if (readOnly) return;
                                if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
                                  event.preventDefault();
                                  move(job, i, i - 1);
                                } else if (event.key === "ArrowDown" || event.key === "ArrowRight") {
                                  event.preventDefault();
                                  move(job, i, i + 1);
                                } else if (event.key === "Delete" || event.key === "Backspace") {
                                  event.preventDefault();
                                  remove(job, i);
                                }
                              }}
                              onDragStart={(event) => {
                                drag.current = { kind: "chip", job, from: i };
                                event.dataTransfer?.setData("text/plain", entry);
                              }}
                              onDragEnd={() => {
                                drag.current = null;
                              }}
                            >
                              <span className="fleet-route-chip-order" aria-hidden>{i + 1}</span>
                              <SourceGlyph kind={reading.kind} size={14} />
                              <span className="fleet-route-chip-name">{reading.label}</span>
                              <span className="fleet-route-chip-state"><ReadinessDot readiness={reading.readiness} /></span>
                            </button>
                            {readOnly ? null : (
                              <IconButton className="fleet-route-chip-remove" label={`Remove ${reading.label} from ${JOB_TITLES[job]}`} onClick={() => remove(job, i)}>
                                <X size={13} aria-hidden />
                              </IconButton>
                            )}
                          </li>
                        );
                      })}
                    </ol>
                  )}
                  {readOnly || selected === null ? null : problem === "" ? (
                    <button type="button" className="fleet-route-job-add" onClick={() => place(job, selected)}>
                      <Plus size={14} aria-hidden /> Add {sourceLabel(selected)}
                    </button>
                  ) : (
                    <p className="fleet-route-job-refused">{problem}</p>
                  )}
                  {refusal?.job === job ? <p className="fleet-route-job-refused" role="alert">{refusal.message}</p> : null}
                  {list.length === 0 ? null : <p className="fleet-route-job-status" data-ready={status.ready || undefined}>{status.text}</p>}
                </div>
              );
            })}
            <EmbeddingsJob embedder={embedder} onOpenModels={onOpenModels} />
          </div>
          {readOnly ? null : <p className="os-caption fleet-assignment-hint">Select a source, then add it to a job. Or drag it into place.</p>}
          <p className="os-caption fleet-route-follows">Images and voice follow Auto.</p>
        </section>
      </div>
    </div>
  );
}

/** Search and memory: the cluster's bound embedder, the same in every route. */
function EmbeddingsJob({ embedder, onOpenModels }: { embedder: EmbedderReading; onOpenModels?: () => void }) {
  return (
    <div className="fleet-route-job" role="group" aria-label={JOB_TITLES.embeddings} data-locked>
      <div className="fleet-assignment-task-name fleet-route-job-name">
        <Search size={16} aria-hidden />
        <span>{JOB_TITLES.embeddings}</span>
        <LockKeyhole size={13} aria-label="The cluster's embedding model" />
      </div>
      <p className="fleet-route-job-meaning">{JOB_MEANING.embeddings}</p>
      {embedder.state === "loading" ? (
        <p className="fleet-route-job-empty"><InlineSkeleton label="Reading the embedding model" /></p>
      ) : embedder.state === "unread" ? (
        <p className="fleet-route-job-empty">Not read{embedder.error ? `: ${embedder.error}` : ""}</p>
      ) : embedder.state === "none" ? (
        <>
          <p className="fleet-route-job-empty">No embedding model yet</p>
          {onOpenModels ? <button type="button" className="os-link fleet-route-job-link" onClick={onOpenModels}>Add one in Models</button> : null}
        </>
      ) : (
        <ol className="fleet-route-job-sources" aria-label="Search and memory model">
          <li className="fleet-route-chip" data-ready>
            <span className="fleet-route-chip-body" aria-label={`${embedder.name}, the cluster's embedding model`}>
              <span className="fleet-route-chip-order" aria-hidden>1</span>
              <SourceGlyph kind="embeddings" size={14} />
              <span className="fleet-route-chip-name">{embedder.name}</span>
            </span>
          </li>
        </ol>
      )}
      <p className="fleet-route-job-status">The same in every route</p>
    </div>
  );
}
