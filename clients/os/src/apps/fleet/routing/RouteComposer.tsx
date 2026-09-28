import { useEffect, useId, useRef, useState } from "react";

import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Head, Notice } from "../../../kit";
import { InfoDetail } from "../../../kit/InfoDetail";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { useOsConnection } from "../../../live/connection";
import { policyDescription, type TaskPolicy } from "../taskPolicies";
import { routeBar, type RouteAct } from "./routeDraft";
import { routeEntries } from "./routes";
import { SourceComposer } from "./SourceComposer";
import { placementProblem, routeStatus, type RoutingFacts } from "./sources";
import { routeTitle } from "./vocabulary";

// A route's own page: the composer, one line of description, and the bar.
//
// It REPLACES the list (DESIGN.md rule 11) with `<- Routes` in the trail, and
// the bar on the window's floor carries every act that changes the route
// (rule 12): the state in words, then Cancel and Restore shipped as text, then
// Save route -- each present only when it is legal.
//
// THE SCOPE IS SAID ONCE, IN THE BAR. A saved route is cluster-wide: every
// rule that takes it takes the change. That used to be a banner above the
// editor that pushed it ~58px down under the cursor; it is the bar's quiet
// detail now, where it is read at the moment of saving and moves nothing.
//
// A DRAFT OUTLIVES THE PAGE. It is held by the list (`draft` / `onDraft`), so
// going back to the list and returning keeps an edit rather than silently
// throwing it away; the row says "Unsaved" meanwhile.

export interface RouteDraftState {
  entries: string[];
  description: string;
}

export function RouteComposer({
  route,
  facts,
  draft,
  onDraft,
  onBack,
  onChanged,
  onAddMachine,
}: {
  route: TaskPolicy;
  facts: RoutingFacts;
  /** The held edit, or undefined when the page shows the route as saved. */
  draft: RouteDraftState | undefined;
  onDraft: (draft: RouteDraftState | undefined) => void;
  onBack: () => void;
  /** Re-read the routes after a write. */
  onChanged: () => void;
  onAddMachine?: () => void;
}) {
  const connection = useOsConnection();
  const title = routeTitle(route.name);
  const saved: RouteDraftState = { entries: routeEntries(route), description: policyDescription(route) };
  const shown = draft ?? saved;
  const differs = draft !== undefined && (JSON.stringify(draft.entries) !== JSON.stringify(saved.entries) || draft.description !== saved.description);
  // THE DRAFT JUST SAVED. It stays on screen until the re-read brings the
  // route back carrying it -- clearing it on the write would flash the OLD
  // chain for the length of a round trip -- and it is not "Unsaved" meanwhile.
  const [committed, setCommitted] = useState("");
  const dirty = differs && JSON.stringify(shown) !== committed;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [confirmingRestore, setConfirmingRestore] = useState(false);
  const [announcement, setAnnouncement] = useState("");

  // A draft that came back EQUAL to the route (an edit undone by hand, or the
  // saved route catching up after a save) is no draft at all.
  useEffect(() => {
    if (draft !== undefined && !differs) onDraft(undefined);
  }, [draft, differs, onDraft]);

  const edit = (next: Partial<RouteDraftState>) => {
    setError("");
    onDraft({ ...shown, ...next });
  };

  const valid = shown.entries.length > 0 && shown.entries.every((entry, i) => placementProblem(entry, shown.entries, route.name, facts, i) === "");
  const status = routeStatus(shown.entries, facts);
  const bar = routeBar({
    shipped: route.shipped === true,
    customized: route.customized === true,
    protected: route.protected === true,
    dirty,
    valid,
    busy,
    confirmingRestore,
    serving: status.word === "" ? "" : status.word.charAt(0).toLowerCase() + status.word.slice(1),
  });

  async function save() {
    if (connection === null || busy) return;
    setBusy(true);
    setError("");
    try {
      await connection.query.routingPolicySave({
        name: route.name,
        description: shown.description,
        primary: shown.entries[0]!,
        fallbacks: shown.entries.slice(1),
        expectedRevision: route.revision ?? 0,
      });
      setCommitted(JSON.stringify(shown));
      setAnnouncement(`${title} saved.`);
      onChanged();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function restore() {
    if (connection === null || busy) return;
    setBusy(true);
    setError("");
    try {
      await connection.query.routingPolicyReset({ name: route.name, expectedRevision: route.revision ?? 0 });
      setConfirmingRestore(false);
      onDraft(undefined);
      setAnnouncement(`${title} uses its shipped sources again.`);
      onChanged();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const acts: Act[] = bar.acts.map((act: RouteAct): Act => {
    switch (act) {
      case "cancel":
        return { label: "Cancel", text: true, onAct: () => { onDraft(undefined); setError(""); } };
      case "restore":
        return { label: "Restore shipped", text: true, onAct: () => setConfirmingRestore(true) };
      case "keep":
        return { label: "Keep", text: true, onAct: () => setConfirmingRestore(false) };
      case "confirmRestore":
        return { label: "Restore shipped", tone: "primary", onAct: () => void restore() };
      case "save":
        return { label: "Save route", tone: "primary", onAct: () => void save() };
    }
  });

  return (
    <div className="os-deploy-pane fleet-route-page">
      <div className="os-deploy-scroll">
        <ActivityTarget target={`fleet:route:${route.name}`}>
          <Head
            title={title}
            breadcrumbs={[{ label: "Routes", onSelect: onBack }, { label: title }]}
            back={{ label: "Routes", onSelect: onBack }}
          >
            <InfoDetail title="How a route works">
              <p>A route tries its sources in order and a call takes the first one that can serve. The line runs as far as a call would get right now and stops at the source that would answer it.</p>
              <p>A source that cannot serve stays in the route and is used again when it can. Vendors bill per call. An app in a route does not grant it permissions on your machine.</p>
            </InfoDetail>
          </Head>
          <Description
            value={shown.description}
            readOnly={route.protected === true}
            onChange={(description) => edit({ description })}
          />
          {error === "" ? null : (
            <Notice tone="error" sentence="The route was not saved." next="Your changes are still here. If it changed elsewhere, cancel to load it." detail={error} />
          )}
          <SourceComposer
            routeName={route.name}
            entries={shown.entries}
            onChange={(entries) => edit({ entries })}
            facts={facts}
            readOnly={route.protected === true}
            onAnnounce={setAnnouncement}
            onAddMachine={onAddMachine}
          />
          <p role="status" className="os-sr-only">{announcement}</p>
        </ActivityTarget>
      </div>
      <ActionBar state={bar.state} detail={bar.detail} tone={bar.tone} acts={acts} />
    </div>
  );
}

/**
 * One line of quiet text, edited in place. Enter or leaving the field keeps
 * the words; Escape puts the old ones back.
 */
function Description({ value, readOnly, onChange }: { value: string; readOnly: boolean; onChange: (next: string) => void }) {
  const id = useId();
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(value);
  const input = useRef<HTMLInputElement>(null);
  // Escape leaves the field, and leaving is a blur; the blur must not keep
  // the words Escape just put back.
  const cancelled = useRef(false);
  useEffect(() => {
    if (editing) input.current?.focus();
  }, [editing]);
  if (readOnly) return value ? <p className="fleet-route-description">{value}</p> : null;
  if (!editing) {
    return (
      <button type="button" className="fleet-route-description" data-empty={value === "" || undefined} aria-label={value ? `Description: ${value}. Edit` : "Add a description"} onClick={() => { cancelled.current = false; setText(value); setEditing(true); }}>
        {value || "Add a description"}
      </button>
    );
  }
  const commit = () => {
    setEditing(false);
    if (cancelled.current) {
      cancelled.current = false;
      return;
    }
    if (text.trim() !== value) onChange(text.trim());
  };
  return (
    <>
      <label className="os-sr-only" htmlFor={id}>Description</label>
      <input
        ref={input}
        id={id}
        className="os-input fleet-route-description-input"
        value={text}
        maxLength={400}
        onChange={(event) => setText(event.target.value)}
        onBlur={commit}
        onKeyDown={(event) => {
          if (event.key === "Enter") {
            event.preventDefault();
            commit();
          } else if (event.key === "Escape") {
            event.preventDefault();
            cancelled.current = true;
            setText(value);
            setEditing(false);
          }
        }}
      />
    </>
  );
}
