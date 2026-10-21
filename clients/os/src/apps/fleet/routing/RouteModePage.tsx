import { useEffect, useState } from "react";

import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Field, Head, Input, Notice } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { JobBoard } from "./JobBoard";
import {
  jobSourcesOf,
  jobsComplete,
  routeModeBar,
  routeNameProblem,
  routeRefusal,
  sameJobSources,
  type EmbedderReading,
  type JobSources,
  type RouteMode,
  type RouteModeAct,
} from "./routeModes";
import type { RoutingFacts } from "./sources";
import type { RouteWrites } from "./useRouteModes";
import { STALE_SENTENCE } from "./vocabulary";

// A route's own page: its jobs and the sources that serve them, and the bar.
//
// It REPLACES the list (DESIGN.md rule 11), with `<- Routes` in the trail, and
// the bar on the window's floor carries every act that changes the route
// (rule 12): Discard changes and Save while there are changes; Restore
// default on a changed built-in; Delete on a route of yours -- each present
// only where it is legal, and Restore and Delete ask first, in the bar.
//
// A DRAFT OUTLIVES THE PAGE. The list holds it (`draft` / `onDraft`), so going
// back and opening the route again keeps an edit; the list row says
// "Unsaved" meanwhile.
//
// WHO MAY CHANGE IT is the route's own answer: a route of yours always, a
// built-in for an owner or developer. Anyone else sees the jobs and what
// serves them, with no tray and no acts -- the bar says who can.

export interface RouteModeDraft {
  jobs: JobSources;
  name: string;
}

export function RouteModePage({
  route,
  facts,
  embedder,
  editable,
  saves,
  revision,
  writes,
  takenNames,
  draft,
  onDraft,
  onBack,
  onChanged,
  onDeleted,
  onAddMachine,
  onOpenModels,
}: {
  route: RouteMode;
  facts: RoutingFacts;
  embedder: EmbedderReading;
  /** Whether this person may change this route. */
  editable: boolean;
  /** Whether this cluster saves routes at all. */
  saves: boolean;
  /** The routing document's revision a built-in write names. */
  revision: number;
  writes: RouteWrites;
  /** The names of the person's other routes, for the rename check. */
  takenNames: readonly string[];
  draft: RouteModeDraft | undefined;
  onDraft: (draft: RouteModeDraft | undefined) => void;
  onBack: () => void;
  /** Read the routes again after a write, landed or refused. */
  onChanged: () => void;
  onDeleted: () => void;
  onAddMachine?: () => void;
  onOpenModels?: () => void;
}) {
  const saved: RouteModeDraft = { jobs: jobSourcesOf(route), name: route.name };
  const shown = draft ?? saved;
  const personal = route.scope === "personal";
  const nameProblem = personal ? routeNameProblem(shown.name, takenNames, route.name) : "";
  const differs = draft !== undefined && (!sameJobSources(draft.jobs, saved.jobs) || (personal && draft.name.trim() !== saved.name));
  // THE DRAFT JUST SAVED stays on screen until the re-read brings the route
  // back carrying it; clearing it on the write would flash the old jobs.
  const [committed, setCommitted] = useState("");
  const dirty = differs && JSON.stringify(shown) !== committed;
  const [busy, setBusy] = useState<"" | "saving" | "restoring" | "deleting">("");
  const [asking, setAsking] = useState<"" | "restore" | "delete">("");
  const [error, setError] = useState<{ stale: boolean; text: string } | null>(null);
  const [announcement, setAnnouncement] = useState("");

  // A draft that came back equal to the route (undone by hand, or the saved
  // route catching up after a save) is no draft at all.
  useEffect(() => {
    if (draft !== undefined && !differs) onDraft(undefined);
  }, [draft, differs, onDraft]);

  const edit = (next: Partial<RouteModeDraft>) => {
    setError(null);
    setAsking("");
    onDraft({ ...shown, ...next });
  };

  const refused = (err: unknown) => {
    setError(routeRefusal(err));
    onChanged();
  };

  async function save() {
    if (busy !== "") return;
    setBusy("saving");
    setError(null);
    try {
      await writes.saveRoute({
        id: route.id,
        ...(personal && shown.name.trim() !== route.name ? { name: shown.name.trim() } : {}),
        jobs: shown.jobs,
        ...(personal ? {} : { revision }),
      });
      setCommitted(JSON.stringify(shown));
      setAnnouncement(`${shown.name.trim() || route.name} saved.`);
      onChanged();
    } catch (err: unknown) {
      refused(err);
    } finally {
      setBusy("");
    }
  }

  async function restore() {
    if (busy !== "" || (route.id !== "local" && route.id !== "apps")) return;
    setBusy("restoring");
    setError(null);
    try {
      await writes.resetRoute(route.id, revision);
      setAsking("");
      onDraft(undefined);
      setAnnouncement(`${route.name} uses its shipped sources again.`);
      onChanged();
    } catch (err: unknown) {
      setAsking("");
      refused(err);
    } finally {
      setBusy("");
    }
  }

  async function remove() {
    if (busy !== "") return;
    setBusy("deleting");
    setError(null);
    try {
      await writes.deleteRoute(route.id);
      onDraft(undefined);
      onDeleted();
    } catch (err: unknown) {
      setAsking("");
      refused(err);
      setBusy("");
    }
  }

  const complete = jobsComplete(shown.jobs) && nameProblem === "";
  const changes = editable && saves;
  const bar = routeModeBar({ route, editable: changes, readOnlyDetail: editable ? "This cluster cannot save routes yet" : undefined, dirty, complete, busy, asking });
  const acts: Act[] = bar.acts.map((act: RouteModeAct): Act => {
    switch (act) {
      case "discard":
        return {
          label: "Discard changes",
          text: true,
          onAct: () => {
            onDraft(undefined);
            if (error !== null) onChanged();
            setError(null);
          },
        };
      case "save":
        return { label: "Save route", tone: "primary", onAct: () => void save() };
      case "restore":
        return { label: "Restore default", text: true, onAct: () => setAsking("restore") };
      case "delete":
        return { label: "Delete", text: true, onAct: () => setAsking("delete") };
      case "keep":
        return { label: "Keep", text: true, onAct: () => setAsking("") };
      case "confirmRestore":
        return { label: "Restore default", tone: "primary", onAct: () => void restore() };
      case "confirmDelete":
        return { label: "Delete route", tone: "danger", onAct: () => void remove() };
    }
  });

  return (
    <div className="os-deploy-pane fleet-route-page fleet-route-mode-page">
      <div className="os-deploy-scroll">
        <ActivityTarget target={`fleet:routes:${route.id}`}>
          <Head navigationKey={`route:${route.id}`} title={shown.name.trim() || route.name} breadcrumbs={[{ label: "Routes", onSelect: onBack }, { label: route.name }]} back={{ label: "Routes", onSelect: onBack }}>
            <PageHelp title="How a route works">
              <p>Each job tries its sources in order and takes the first one that can serve right now. A source that cannot serve stays in the job and is used again when it can.</p>
              <p>Search and memory always uses the cluster's embedding model, so everything stays in one index. Images and voice follow Auto. Vendors bill per call.</p>
            </PageHelp>
          </Head>
          <p className="fleet-route-description">{route.description}</p>
          {route.ready || route.reason === "" ? null : <p className="fleet-route-reason">{route.reason}</p>}
          {error === null ? null : error.stale ? (
            <Notice tone="warn" sentence={STALE_SENTENCE} next="It has been read again. Discard to see the route as it is now, or save to replace it." />
          ) : (
            <Notice tone="error" sentence="The route was not saved." next="Your changes are still here." detail={error.text} />
          )}
          {personal && changes ? (
            <div className="fleet-route-name">
              <Field label="Name">
                <Input id={`fleet-route-name-${route.id}`} label="Name" value={shown.name} onChange={(name) => edit({ name })} />
              </Field>
              {nameProblem === "" ? null : <p className="fleet-route-job-refused" role="status">{nameProblem}</p>}
            </div>
          ) : null}
          <JobBoard
            jobs={shown.jobs}
            onChange={(jobs) => edit({ jobs })}
            facts={facts}
            embedder={embedder}
            readOnly={!changes}
            onAnnounce={setAnnouncement}
            onAddMachine={onAddMachine}
            onOpenModels={onOpenModels}
          />
          <p role="status" className="os-sr-only">{announcement}</p>
        </ActivityTarget>
      </div>
      <ActionBar state={bar.state} detail={bar.detail} tone={bar.tone} acts={acts} scope={`route:${route.id}`} />
    </div>
  );
}
