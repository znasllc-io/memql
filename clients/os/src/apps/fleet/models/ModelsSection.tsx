import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { LocalTabs } from "../../../kit/LocalTabs";
import { useMemo, useState } from "react";
import { RefreshButton, useFleetScroll } from "../FleetControls";
import { InfoDetail } from "../../../kit/InfoDetail";

import {
  Caption,
  Switch,
  EmptyState,
  Button,
  Chip,
  Chips,
  Fact,
  Facts,
  Head,
  Measure,
  Notice,
  Panel,
  Refine,
  Select,
  Subhead,
  RecordList,
  RecordRow,
} from "../../../kit";
import { figureFrom, type Figure } from "../../../kit/measure";
import { CatalogSection } from "./CatalogSection";
import { eligibleFor, formatContext, formatParams, orderModels, type ModelNeeds } from "./ordering";
import { useInference, type CatalogModel, type DoorsReading } from "./useInference";

// Models: what this fleet can actually serve, in the order the router would
// pick from (epic memql#5096).
//
// ===========================================================================
// THE ORDER IS THE POINT
// ===========================================================================
// Every other list in this app answers "what do I have". This one answers
// "how my preferences and model sizes compare". The first eligible row in
// that order is marked, but measured structured-output validity can change
// the engine's actual strongest selection. The caption states that boundary
// rather than presenting this display as a dispatch prediction.
//
// It is not alphabetical, and the shape of the screen is what says so.
//
// ===========================================================================
// WHY A TURN KIND CHANGES THE ANSWER
// ===========================================================================
// A structured turn needs a model that advertises structured output; a tool
// turn needs one that advertises tool calling; an embedding turn needs a
// different model entirely. So there is no single "next model" -- there is one
// per kind of turn, and printing a single one would be confidently wrong for
// two thirds of the traffic. The header row names all four.

/**
 * What has been measured about a model, or the reason nothing has.
 *
 * `measured` is filled by epic memql#5146's probe and is ABSENT everywhere
 * until it lands. That absence is a value, not a gap: a machine nobody has
 * probed showing "0 tok/s" is worse than showing nothing, because a zero says
 * "we measured, and the answer is none" -- which for a throughput figure is a
 * claim that the model does not work.
 *
 * `figureFrom` reads an absent key as `unmeasured` rather than as a number, so
 * this is total against a wire that does not carry the field yet.
 */
function measuredOf(model: CatalogModel): Figure {
  return figureFrom(model as unknown as Record<string, unknown>, "measuredTokensPerSecond");
}

const CAPABILITY_LABEL: Record<string, string> = {
  structured: "structured output",
  tools: "tool calling",
  embeddings: "embeddings",
};

/** The turns the platform makes, and what each needs of a model. */
const TURNS: Array<{ id: string; label: string; needs: ModelNeeds }> = [
  { id: "chat", label: "Chat", needs: {} },
  { id: "structured", label: "Structured", needs: { structuredOutput: true } },
  { id: "tools", label: "Tool calling", needs: { tools: true } },
  { id: "embedding", label: "Embeddings", needs: { embeddings: true } },
];

export function ModelsSection({ onHome }: { onHome?: () => void } = {}) {
  const { catalog, doors, profiles, preference } = useInference();
  const models = catalog.value ?? [];
  const [catalogSearch, setCatalogSearch] = useState("");
  const reading = catalog.state === "reading" || doors.state === "reading";

  // REFINE, not a standing filter strip (DESIGN.md rule 2): collapsed until
  // asked, active constraints as removable chips beside it, and never shown
  // over an empty list.
  //
  // THESE ARE THE FACETS OVER THE RANKED LIST, AND ONLY OVER IT. Epic
  // memql#5137's catalog landed with its own category / runtime / lacking
  // facets behind a `facets` prop on CatalogSection, deliberately NOT wired
  // into this control -- an earlier note here anticipated one Refine governing
  // both lists, and building it made the reason not to obvious. Half of these
  // chips would narrow the list above and none of the catalog, and half the
  // reverse; a chip that says "online" while the catalog below it is unchanged
  // is a control lying about what it did, which is the failure mode the same
  // note was written to avoid. Two lists that answer different questions get
  // two controls, or one control whose every chip governs both.
  const [view, setView] = useState<"available" | "catalog" | "sources">("available");
  const root = useFleetScroll(view);
  const [category, setCategory] = useState("");
  const [runtime, setRuntime] = useState("");
  const [lackingOnly, setLackingOnly] = useState(false);
  const [search, setSearch] = useState("");
  const [capability, setCapability] = useState("");
  const [onlineOnly, setOnlineOnly] = useState(false);

  // ORDERED ONCE, here, and rendered in that order. Every "which model" answer
  // below reads this array rather than re-sorting, so the marks and the list
  // cannot disagree about the ranking.
  const ranked = useMemo(() => orderModels(models, preference), [models, preference]);

  // THE RANKS ARE THE FULL LIST'S, NOT THE FILTERED VIEW'S. A model's rank is
  // its position in what the router would pick from, so renumbering a narrowed
  // view would print a different answer to the question this screen exists to
  // answer -- "rank 1" under a filter would name a model the router reaches
  // third. The filter hides rows; it never renumbers them.
  const shown = useMemo(
    () =>
      ranked.filter((m) => {
        if (onlineOnly && !m.online) return false;
        if (capability === "structured" && !m.structuredOutput) return false;
        if (capability === "tools" && !m.tools) return false;
        if (capability === "embeddings" && !m.embeddings) return false;
        const q = search.trim().toLowerCase();
        return q === "" || m.modelId.toLowerCase().includes(q);
      }),
    [ranked, onlineOnly, capability, search],
  );

  // Whether ANY model on this fleet has been measured. When none has, the
  // column is absent and one sentence says why -- a measured column of
  // forty-four identical absence marks is forty-four things to read past. When
  // SOME have, an unmeasured row shows its absence mark, because somebody
  // scanning a column of figures for the one that is missing has to see the
  // gap.
  const anyMeasured = useMemo(
    () => ranked.some((m) => measuredOf(m).kind === "measured"),
    [ranked],
  );

  // The first eligible model per turn kind. `null` when the fleet cannot serve
  // that kind at all, which is a state the header states rather than hides.
  const nextByTurn = useMemo(() => {
    const out = new Map<string, string>();
    for (const turn of TURNS) {
      const hit = ranked.find((m) => eligibleFor(m, turn.needs).ok);
      if (hit) out.set(turn.id, hit.modelId);
    }
    return out;
  }, [ranked]);

  return (
    <div ref={root} className="os-fleet">
      <div className="fleet-section-header">
      <Head
        title="Model library"
        meta={view === "available" && catalog.state === "read" && !catalog.error ? shown.length : undefined}
      >
        {/* A REFRESH CONTROL BELONGS HERE, unlike on the live sections: both
            readings are on-demand projections that are never broadcast, so
            offering to look again is the honest affordance rather than a
            contradiction of a feed that arrives on its own. */}
        <RefreshButton label="Refresh model library" busy={reading} onClick={() => { catalog.reread(); doors.reread(); profiles.reread(); }} />
      </Head>

      <LocalTabs label="Model library views" value={view} onChange={setView} options={[["available", "Available models"], ["catalog", "Catalog"], ["sources", "Inference sources"]]} />
      </div>
      <div hidden={view !== "sources"}><DoorsPanel doors={doors.value} state={doors.state} error={doors.error} /></div>
      <div hidden={view !== "available"}>

      {catalog.state === "failed" ? (
        <Notice
          tone="info"
          sentence="We could not read your fleet's catalog."
          next="Refresh to try again. If it still fails, check Fleet logs."
          detail={catalog.error}
        />
      ) : null}

      <Subhead>Ranked for your fleet</Subhead>

      {/* NOTHING ABOUT THE ORDER IS SHOWN OVER AN EMPTY LIST. The ranking
          rule, the preference chips and the four turn lines all describe an
          ordering of models, and printing them above nothing describes an
          order of nothing -- the same reason a section never shows filter
          chrome over no content (rule 2). The empty notice carries the whole
          message on its own. */}
      {models.length === 0 ? null : (
        <>
          <InfoDetail title="Model ranking"><Caption>
            This list shows your preferences, then active parameters per token (total when
            unreported), context window, and model id. A model that did not report its size sorts last.
            For <span className="os-mono">fleet:strongest</span>, measured structured-output reliability
            takes priority over preferences and size, so the model selected for a call can differ.
          </Caption></InfoDetail>

          {preference.length > 0 ? (
            <Chips label="Your preferred order">
              {preference.map((id, i) => (
                <Chip key={`${id}:${i}`} tone="accent">
                  {id}
                </Chip>
              ))}
            </Chips>
          ) : null}

          <Caption>Highest-ranked compatible models:</Caption>
          <NextForEachTurn next={nextByTurn} known />

          <div className="os-fleet-models-scope">
            <Refine
              label="Refine the ranked list"
              search={search}
              onSearch={setSearch}
              placeholder="Search"
              chips={[
                ...(capability === ""
                  ? []
                  : [
                      {
                        id: "capability",
                        label: CAPABILITY_LABEL[capability] ?? capability,
                        onRemove: () => setCapability(""),
                      },
                    ]),
                ...(onlineOnly
                  ? [{ id: "online", label: "online now", onRemove: () => setOnlineOnly(false) }]
                  : []),
              ]}
            >
              <Select
                id="models-facet-capability"
                label="Capability"
                value={capability}
                onChange={setCapability}
              >
                <option value="">Any capability</option>
                <option value="structured">Structured output</option>
                <option value="tools">Tool calling</option>
                <option value="embeddings">Embeddings</option>
              </Select>
              <Switch checked={onlineOnly} onChange={setOnlineOnly}>
                Online now
              </Switch>
            </Refine>
          </div>
        </>
      )}

      {catalog.state === "reading" && models.length === 0 ? <RecordListSkeleton label="Loading available models" rows={3} /> : null}
      {catalog.state === "read" && ranked.length === 0 ? (
        <EmptyState title="No models available" action={<><Button onClick={() => setView("catalog")}>Browse model catalog</Button>{onHome ? <Button onClick={onHome}>Go to Machines</Button> : null}</>}>Connect a machine and install a local model to make it available here.</EmptyState>
      ) : null}

      {ranked.length > 0 && shown.length === 0 ? (
        <EmptyState title="No matching models" action={<Button onClick={() => { setSearch(""); setCapability(""); setOnlineOnly(false); }}>Clear filters</Button>}>Try another name or include more capabilities and offline machines.</EmptyState>
      ) : null}

      <RecordList as="ul" label="Available models">
        {shown.map((model) => (
          <ModelLine
            key={model.modelId}
            model={model}
            rank={ranked.indexOf(model) + 1}
            preferred={preference.includes(model.modelId)}
            serves={TURNS.filter((t) => nextByTurn.get(t.id) === model.modelId).map((t) => t.label)}
            measured={measuredOf(model)}
            showMeasured={anyMeasured}
          />
        ))}
      </RecordList>

      {/* SAID ONCE, UNDER THE LIST, rather than as a column of identical
          absence marks. When the probe lands (epic memql#5146) the column
          appears and this line goes away on its own. */}
      {ranked.length > 0 && !anyMeasured ? (
        <Caption>
          Performance has not been measured yet. Open a machine’s Models tab to run a probe.
        </Caption>
      ) : null}

      {catalog.at === null ? null : (
        <Caption>Read {catalog.at.toLocaleTimeString()}.</Caption>
      )}

      {/* THE CATALOG COMES SECOND, and the order is the argument. The list
          above answers "what will be used", which is what somebody opens this
          page to find out. This answers "what should I be running", which is
          the question they have once they have seen the answer to the first
          one -- and putting it first would make every visit start with a
          recommendation nobody asked for. */}
      </div>
      <div hidden={view !== "catalog"}>
      {(profiles.value?.length ?? 0) > 0 ? <Refine label="Refine model catalog" search={catalogSearch} onSearch={setCatalogSearch} chips={[
        ...(category ? [{ id: "category", label: category, onRemove: () => setCategory("") }] : []),
        ...(runtime ? [{ id: "runtime", label: runtime, onRemove: () => setRuntime("") }] : []),
        ...(lackingOnly ? [{ id: "gaps", label: "Capability gaps", onRemove: () => setLackingOnly(false) }] : []),
      ]}><div className="fleet-catalog-filters">
        <Select id="fleet-catalog-category" label="Catalog category" value={category} onChange={setCategory}><option value="">All categories</option>{Array.from(new Set((profiles.value ?? []).map(p => p.category))).map(c => <option key={c} value={c}>{c}</option>)}</Select>
        <Select id="fleet-catalog-runtime" label="Catalog runtime" value={runtime} onChange={setRuntime}><option value="">All runtimes</option>{Array.from(new Set((profiles.value ?? []).map(p => p.runtime))).map(r => <option key={r} value={r}>{r}</option>)}</Select>
        <Switch checked={lackingOnly} onChange={setLackingOnly}>Capability gaps</Switch>
      </div></Refine> : null}
      <CatalogSection
        onClearFilters={() => { setCatalogSearch(""); setCategory(""); setRuntime(""); setLackingOnly(false); }}
        facets={{ search: catalogSearch, category, runtime, lackingOnly }}
        profiles={profiles.value ?? []}
        profilesState={profiles.state}
        profilesError={profiles.error}
        fleet={models}
      />
      </div>
    </div>
  );
}

/**
 * Which doors this cluster can reach, in the order the default chain tries
 * them. It is supporting context for the list below, so it sits in a Panel.
 */
function DoorsPanel({
  doors,
  state,
  error,
}: {
  doors: DoorsReading | null;
  state: string;
  error: string;
}) {
  return (
    <Panel label="Inference sources">
      {state === "failed" ? (
        <Notice
          tone="info"
          sentence="Inference sources could not be read."
          next="Refresh to check source availability again."
          detail={error}
        />
      ) : null}
      {doors === null ? (
        state === "reading" ? <RecordListSkeleton label="Loading the cluster" /> : null
      ) : (
        <>
          <p className="os-cluster-fact">{doorSentence(doors)}</p>
          <div className="os-fleet-doors">
            <DoorState
              name="Local model"
              open={doors.localEligible}
              detail={
                doors.localEligible
                  ? `${doors.eligibleModelIds.length} of ${doors.localModelCount} meet the ${doors.minimumContextWindow.toLocaleString()}-token floor`
                  : doors.fleetCatalogInstalled
                    ? doors.localModelCount === 0
                      ? "your fleet offers no models"
                      : "nothing meets the floor with structured output"
                    : "fleet inventory cannot be read here"
              }
            />
            <DoorState
              name="Signed-in app"
              open={doors.appEligible}
              detail={
                doors.appEligible
                  ? doors.runnableApps.join(", ")
                  : doors.appSessionsInstalled
                    ? "no machine has one allowed, signed in and online here"
                    : "this node cannot open app sessions at all"
              }
            />
            <DoorState
              name="Federation"
              open={doors.federationConfigured}
              detail={
                doors.federationConfigured
                  ? "workload-identity federation"
                  : doors.cloudConfigured
                    ? // A callable cloud provider that is not federated cannot
                      // happen since epic memql#5088, and this is the one line
                      // that would notice if that stopped being true.
                      //
                      // THE INVARIANT IT RESTS ON LIVES IN ANOTHER FILE: a
                      // vendor entry becomes Available only after resolvedAuth
                      // and newAIProvider both succeed
                      // (component/memql/unified_kinds_loader.go), and with the
                      // key tier deleted federation is the only path either can
                      // take -- so Available implies federated. Before that
                      // deletion this was NOT unreachable but ORDINARY: a
                      // developer running `make up` with a static key had
                      // cloudConfigured and no federation, and would have read
                      // this on every load.
                      //
                      // So if a static-key path ever returns -- a local-dev
                      // break-glass is the likely shape -- this line starts
                      // warning about clusters that are fine. Restore one and
                      // you owe this sentence an edit.
                      "a cloud provider is callable but not through federation"
                    : "not configured"
              }
            />
          </div>
          <Caption>
            Source availability and policy preference are separate. A configured source must still be compatible with the call.
          </Caption>
        </>
      )}
    </Panel>
  );
}

function DoorState({ name, open, detail }: { name: string; open: boolean; detail: string }) {
  return (
    <div className="os-fleet-door" data-open={open || undefined}>
      <span className="os-fleet-door-name">{name}</span>
      <span className="os-fleet-door-state">{open ? "Available" : "Unavailable"}</span>
      <span className="os-caption">{detail}</span>
    </div>
  );
}

/**
 * One line per kind of turn, naming the model that would serve it.
 *
 * FOUR ANSWERS RATHER THAN ONE, because a structured turn and an embedding
 * turn legitimately resolve to different models on the same fleet, and a
 * single "next model" would be confidently wrong for most of the traffic.
 */
function NextForEachTurn({ next, known }: { next: Map<string, string>; known: boolean }) {
  if (!known) return null;
  return (
    <div className="os-fleet-turns">
      {TURNS.map((turn) => {
        const model = next.get(turn.id);
        return (
          <div className="os-fleet-turn" key={turn.id} data-served={model ? true : undefined}>
            <span className="os-fleet-turn-kind">{turn.label}</span>
            <span className={model ? "os-mono" : "os-caption"}>
              {model ?? "nothing on your fleet can serve this"}
            </span>
          </div>
        );
      })}
    </div>
  );
}

function ModelLine({
  model,
  rank,
  preferred,
  serves,
  measured,
  showMeasured,
}: {
  model: CatalogModel;
  rank: number;
  preferred: boolean;
  serves: string[];
  measured: Figure;
  /** Whether ANY model on this fleet is measured -- see `anyMeasured`. */
  showMeasured: boolean;
}) {
  const [open, setOpen] = useState(false);
  const size = formatParams(model.params);
  const window = formatContext(model.contextWindow);

  return (
    <div>
      <RecordRow name={model.modelId} icon={<span aria-hidden>{rank}</span>}
        secondary={serves.length ? `next for ${serves.join(", ").toLowerCase()}` : undefined}
        state={model.online ? "Online" : "Offline"} tone={model.online ? "accent" : "muted"}
        open={open} onOpen={() => setOpen(value => !value)}>
        {preferred ? <Chip tone="accent">preferred</Chip> : null}
        <span>{size}</span><span>{window ? `${window} tokens` : ""}</span>
      </RecordRow>
      {open ? <div className="fleet-model-detail">
      <Facts>
        {/* SIZE IS NOT PRINTED AS ZERO. Zero parameters is not a thing, and a
            "0" here would make the unmeasured model look like the smallest
            rather than the one that did not say -- which is precisely the
            distinction the ordering rule turns on. */}
        <Fact
          label="Size"
          value={size === "" ? "not reported — sorts last" : size}
          mono={size !== ""}
        />
        <Fact label="Context" value={window === "" ? "not reported" : `${window} tokens`} />
        {model.quant === "" ? null : <Fact label="Quantization" value={model.quant} mono />}
        <Fact
          label="Machines"
          value={
            model.machineCount === 0
              ? "none"
              : `${model.onlineCount} of ${model.machineCount} online`
          }
        />
        {/* MEASURED, and only once something on this fleet has been. An
            unmeasured row here draws `Measure`'s absence mark rather than a
            zero -- the gap is visible on purpose, because somebody scanning
            the column for what is missing has to be able to see it. */}
        {showMeasured ? (
          <Fact label="Measured" value={<Measure figure={measured} suffix=" tok/s" />} />
        ) : null}
      </Facts>

      <Chips label="Capabilities">
        <Chip tone={model.structuredOutput ? "accent" : "muted"}>
          {model.structuredOutput ? "structured output" : "no structured output"}
        </Chip>
        <Chip tone={model.tools ? "accent" : "muted"}>
          {model.tools ? "tool calling" : "no tool calling"}
        </Chip>
        <Chip tone={model.embeddings ? "accent" : "muted"}>
          {model.embeddings ? "embeddings" : "no embeddings"}
        </Chip>
      </Chips>

      {model.machines.length === 0 ? null : (
        <RecordList as="ul" label="Machines serving this model">
          {model.machines.map((machine) => (
            <RecordRow key={machine.registrationId} name={machine.displayName || machine.name || machine.registrationId} state={machine.online ? (machine.busy ? "Busy" : "Online") : "Offline"}>
              <span className="os-caption">
                {machine.online ? (machine.busy ? "busy" : "online") : "offline"}
                {machine.maxConcurrent > 0
                  ? ` · ${machine.activeCount} of ${machine.maxConcurrent} calls`
                  : ""}
                {machine.runtimes.length > 0 ? ` · ${machine.runtimes.join(", ")}` : ""}
              </span>
            </RecordRow>
          ))}
        </RecordList>
      )}
      </div> : null}
    </div>
  );
}

/** The doors in a sentence, because the first question is not a list. */
function doorSentence(doors: DoorsReading): string {
  const open = doors.doorsOpen.map(doorWord);
  if (open.length === 0) {
    return "No inference source is available. Connect a model or a signed-in app to run tasks that need inference.";
  }
  if (open.length === 1) return `This cluster reaches a model through ${open[0]}.`;
  return `This cluster reaches a model through ${open.slice(0, -1).join(", ")} and ${open[open.length - 1]}, in that order.`;
}

/** The doors in the reader's words. An unrecognised value is printed as it
 *  came, never dropped: a door this build has no name for is still a door. */
function doorWord(door: string): string {
  switch (door) {
    case "local":
      // Yours, or lent to you (epic memql#5344): a person's catalog holds
      // both, and somebody with no machine of their own may be using one.
      return "a local model on your machines or on one lent to you";
    case "app":
      return "a signed-in app on one of your machines";
    case "federation":
      return "workload-identity federation";
    // `apiKey` has no case, because it has no producer: the door went with the
    // vendor keys (epic memql#5088). An older node still reporting it falls
    // through to the pass-through below and is printed as it came, which is
    // the right treatment for a value from a build this one does not know.
    default:
      return door;
  }
}
