import { useState } from "react";
import { Route } from "lucide-react";

import { Field, Input, Notice } from "../../../kit";
import type { Act } from "../../../kit/ActionBar";
import type { Stop } from "../../../kit/Rail";
import { Wizard } from "../../../kit/Wizard";
import { useOsConnection } from "../../../live/connection";
import type { TaskPolicy } from "../taskPolicies";
import { catalogRevision } from "./routes";
import { GlyphStrip } from "./SourceGlyph";
import { SourceComposer } from "./SourceComposer";
import { placementProblem, readSource, routeStatus, servingIndex, type RoutingFacts } from "./sources";
import { STALE_SENTENCE, refusalWords, routeIdFrom } from "./vocabulary";

// New route: Name -> Sources -> Review, on the shell's one wizard (DESIGN.md,
// "One wizard for adding"). The title is the Add control's own words. Each
// step's forward act is on the floor and nowhere else, and is absent until the
// step is answered. The Sources step IS the composer -- the same slots, tray
// and circuit a saved route is edited with -- so there is one way to build a
// chain, learnt once.

type Step = "name" | "sources" | "review";

export function NewRouteWizard({
  routes,
  facts,
  onCancel,
  onCreated,
  onRefused,
  onAddMachine,
}: {
  routes: readonly TaskPolicy[];
  facts: RoutingFacts;
  onCancel: () => void;
  onCreated: (name: string) => void;
  /** Re-read routes and rules after a refused save: they share one revision. */
  onRefused?: () => void;
  onAddMachine?: () => void;
}) {
  const connection = useOsConnection();
  const [step, setStep] = useState<Step>("name");
  const [name, setName] = useState("");
  const [entries, setEntries] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ stale: boolean; text: string } | null>(null);
  const [announcement, setAnnouncement] = useState("");

  const id = routeIdFrom(name);
  const taken = id !== "" && routes.some((r) => r.name === id);
  const named = id !== "" && !taken;
  const chained = entries.length > 0 && entries.every((entry, i) => placementProblem(entry, entries, id, facts, i) === "");
  const status = routeStatus(entries, facts);

  async function save() {
    if (connection === null || busy) return;
    setBusy(true);
    setError(null);
    try {
      await connection.query.routingPolicySave({
        name: id,
        description: "",
        primary: entries[0]!,
        fallbacks: entries.slice(1),
        expectedRevision: catalogRevision(routes),
      });
      onCreated(id);
    } catch (err: unknown) {
      setError(refusalWords(err instanceof Error ? err.message : String(err)));
      onRefused?.();
    } finally {
      setBusy(false);
    }
  }

  const reached = (s: Step) => s === "name" || (s === "sources" && named) || (s === "review" && named && chained);
  const stateOf = (s: Step): Stop["state"] =>
    s === step ? "open" : reached(s) ? (["name", "sources", "review"].indexOf(s) < ["name", "sources", "review"].indexOf(step) ? "done" : "waiting") : "ahead";

  const steps: Stop[] = [
    {
      id: "name",
      name: "Name",
      state: stateOf("name"),
      sentence: "What to call it.",
      answer: named ? name.trim() : undefined,
      body: (
        <Field label="Route name">
          <Input id="fleet-new-route-name" label="Route name" value={name} placeholder="Night shift" onChange={setName} onEnter={() => { if (named) setStep("sources"); }} />
        </Field>
      ),
    },
    {
      id: "sources",
      name: "Sources",
      state: stateOf("sources"),
      sentence: "Tried in order until one can serve.",
      answer: entries.length > 0 ? `${entries.length} ${entries.length === 1 ? "source" : "sources"}` : undefined,
      body: (
        <SourceComposer
          routeName={id}
          entries={entries}
          onChange={setEntries}
          facts={facts}
          onAnnounce={setAnnouncement}
          onAddMachine={onAddMachine}
        />
      ),
    },
    {
      id: "review",
      name: "Review",
      state: stateOf("review"),
      body: (
        <div className="fleet-route-review">
          <strong>{name.trim()}</strong>
          <GlyphStrip readings={entries.map((entry) => readSource(entry, facts))} servingIndex={servingIndex(entries, facts).index} />
          <span>{status.word}</span>
        </div>
      ),
    },
  ];

  const forward: Act | null =
    step === "name"
      ? named ? { label: "Next", tone: "primary", onAct: () => setStep("sources") } : null
      : step === "sources"
        ? chained ? { label: "Next", tone: "primary", onAct: () => setStep("review") } : null
        : { label: "Save route", tone: "primary", busy, onAct: () => void save() };

  const statusWord =
    step === "name"
      ? { word: "Name the route", detail: taken ? "A route with that name exists" : "" }
      : step === "sources"
        ? { word: entries.length === 0 ? "Add a source" : "Sources", detail: entries.length === 0 ? "" : status.word }
        : { word: busy ? "Saving" : "Ready to save", detail: "" };

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
        status={{ word: statusWord.word, detail: statusWord.detail, tone: busy ? "busy" : "none" }}
        acts={[{ label: "Cancel", text: true, onAct: onCancel }, ...(forward ? [forward] : [])]}
        notices={
          error === null ? null : error.stale ? (
            <Notice tone="warn" sentence={STALE_SENTENCE} next="Nothing was written. It has been read again, so saving now uses what is there." />
          ) : (
            <Notice tone="error" sentence="The route was not added." next="Nothing was written. The cluster's own reason is below." detail={error.text} />
          )
        }
        context={{ page: "New route", step }}
      />
      <p role="status" className="os-sr-only">{announcement}</p>
    </>
  );
}
