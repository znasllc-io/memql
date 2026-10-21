import { useState } from "react";
import { Route } from "lucide-react";

import { ChoiceStack, Field, Input, Notice } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { JobBoard } from "./JobBoard";
import {
  EDITABLE_JOBS,
  EMPTY_JOBS,
  SHIPPED_JOBS,
  jobSourcesOf,
  jobsComplete,
  routeNameProblem,
  routeRefusal,
  type EmbedderReading,
  type JobSources,
  type RouteMode,
} from "./routeModes";
import type { RoutingFacts } from "./sources";
import type { RouteWrites } from "./useRouteModes";
import { STALE_SENTENCE } from "./vocabulary";

// New route: Name -> Start from -> Jobs, on the shell's one wizard (DESIGN.md,
// "One wizard for adding"). The title is the Add control's own words, each
// step's forward act is on the floor and nowhere else, and "Start from" is
// one choice, answered by choosing.
//
// STARTING FROM LOCAL OR APPS COPIES THEIR SOURCES AS THEY ARE NOW, on this
// cluster: a later change to Local or Apps does not reach a route made from
// them. Starting empty leaves every job to fill.

type Step = "name" | "start" | "jobs";
type Start = "local" | "apps" | "empty";

const STARTS = [
  { value: "local", label: "Local", description: "Local models for every job" },
  { value: "apps", label: "Apps", description: "Claude Code or Codex for every job" },
  { value: "empty", label: "Empty", description: "Choose every source yourself" },
] as const;

export function NewRouteModeWizard({
  routes,
  facts,
  embedder,
  writes,
  onCancel,
  onCreated,
  onRefused,
  onAddMachine,
  onOpenModels,
}: {
  routes: readonly RouteMode[];
  facts: RoutingFacts;
  embedder: EmbedderReading;
  writes: RouteWrites;
  onCancel: () => void;
  onCreated: (id: string) => void;
  /** Read the routes again after a refused save. */
  onRefused?: () => void;
  onAddMachine?: () => void;
  onOpenModels?: () => void;
}) {
  const [step, setStep] = useState<Step>("name");
  const [name, setName] = useState("");
  const [start, setStart] = useState<Start | "">("");
  const [jobs, setJobs] = useState<JobSources>(EMPTY_JOBS);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ stale: boolean; text: string } | null>(null);
  const [announcement, setAnnouncement] = useState("");

  const taken = routes.filter((r) => r.scope === "personal").map((r) => r.name);
  const nameProblem = routeNameProblem(name, taken);
  const named = nameProblem === "";
  const complete = jobsComplete(jobs);
  const filled = EDITABLE_JOBS.filter((job) => jobs[job].length > 0).length;

  function startFrom(next: Start) {
    setStart(next);
    const source = next === "empty" ? EMPTY_JOBS : jobSourcesOf(routes.find((r) => r.id === next && r.scope === "cluster") ?? { jobs: [] });
    // The engine's copy when it has one, the shipped definition when it does not.
    const copied = next !== "empty" && EDITABLE_JOBS.every((job) => source[job].length === 0) ? SHIPPED_JOBS[next] : source;
    setJobs({ fast: [...copied.fast], strong: [...copied.strong], reasoning: [...copied.reasoning] });
    setStep("jobs");
  }

  async function save() {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const created = await writes.saveRoute({ name: name.trim(), jobs });
      onCreated(created?.id ?? "");
    } catch (err: unknown) {
      setError(routeRefusal(err));
      onRefused?.();
    } finally {
      setBusy(false);
    }
  }

  const order: Step[] = ["name", "start", "jobs"];
  const reached = (s: Step) => s === "name" || (s === "start" && named) || (s === "jobs" && named && start !== "");
  const stateOf = (s: Step): Stop["state"] =>
    s === step ? "open" : reached(s) ? (order.indexOf(s) < order.indexOf(step) ? "done" : "waiting") : "ahead";

  const steps: Stop[] = [
    {
      id: "name",
      name: "Name",
      sentence: "Name and purpose",
      state: stateOf("name"),
      answer: named && step !== "name" ? name.trim() : undefined,
      body: (
        <Field label="Route name">
          <Input id="fleet-new-route-mode-name" label="Route name" value={name} placeholder="Out of credits" onChange={setName} onEnter={() => { if (named) setStep("start"); }} />
        </Field>
      ),
    },
    {
      id: "start",
      name: "Start from",
      sentence: "Copy Local, Apps or start empty",
      state: stateOf("start"),
      answer: start === "" || step === "start" ? undefined : STARTS.find((s) => s.value === start)?.label,
      body: (
        <ChoiceStack
          name="fleet-new-route-start"
          label="Start from"
          voice="prose"
          value={start}
          onChange={(next) => startFrom(next as Start)}
          options={STARTS.map((s) => ({ value: s.value, label: s.label, description: s.description }))}
        />
      ),
    },
    {
      id: "jobs",
      name: "Jobs",
      sentence: "Choose a model for each job",
      state: stateOf("jobs"),
      answer: step === "jobs" ? undefined : `${filled} of ${EDITABLE_JOBS.length} jobs`,
      body: (
        <JobBoard jobs={jobs} onChange={setJobs} facts={facts} embedder={embedder} onAnnounce={setAnnouncement} onAddMachine={onAddMachine} onOpenModels={onOpenModels} />
      ),
    },
  ];

  const forward: Act | null =
    step === "name"
      ? named ? { label: "Continue", tone: "primary", onAct: () => setStep("start") } : null
      : step === "start"
        ? null
        : complete ? { label: "Save route", tone: "primary", busy, onAct: () => void save() } : null;

  const status =
    step === "name"
      ? { word: named ? "Ready to continue" : "Name your route", detail: name.trim() === "" ? "" : nameProblem }
      : step === "start"
        ? { word: "Choose where to start", detail: "" }
        : { word: busy ? "Saving" : complete ? "Ready to save" : "Choose a source for each job", detail: `${filled} of ${EDITABLE_JOBS.length} jobs` };

  return (
    <>
      <Wizard
        className="fleet-route-wizard"
        icon={<Route aria-hidden />}
        title="New route"
        breadcrumbs={[{ label: "Routes", onSelect: onCancel }, { label: "New route" }]}
        back={{ label: "Routes", onSelect: onCancel }}
        label="Adding a route"
        steps={steps}
        open={step}
        onOpen={(next) => {
          if (reached(next as Step)) setStep(next as Step);
        }}
        status={{ word: status.word, detail: status.detail, tone: busy ? "busy" : "none" }}
        acts={[{ label: "Cancel", text: true, onAct: onCancel }, ...(forward ? [forward] : [])]}
        notices={
          error === null ? null : error.stale ? (
            <Notice tone="warn" sentence={STALE_SENTENCE} next="Nothing was written. It has been read again." />
          ) : (
            <Notice tone="error" sentence="The route was not added." next="Nothing was written." detail={error.text} />
          )
        }
        context={{ page: "New route", step }}
      />
      <p role="status" className="os-sr-only">{announcement}</p>
    </>
  );
}
