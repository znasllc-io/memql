import { useEffect, useMemo, useRef, useState } from "react";

import { Caption, Check, Head, Panel, Select } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import { useOsIfPresent } from "../../chrome/state";
import type { OsAppProps } from "../../system/registry";
import { ComposerSection } from "./ComposerSection";
import { MaterializedSection } from "./MaterializedSection";
import { TemplatesSection } from "./TemplatesSection";
import { MATERIALIZER_APP_ID, MATERIALIZER_LOG_CONCEPTS } from "./concepts";
import { GOAL_APP_ID, GOAL_APP_SECTION, goalIntent } from "./handoff";
import {
  useCompositionActs,
  useMaterialize,
  useRecipeActs,
  useTemplateActs,
} from "./actions";
import {
  compositionFromRow,
  modelsOf,
  recipeFromRow,
  sourcesOf,
  templateFromRow,
  type CompositionRow,
  type ModelRow,
  type RecipeRow,
  type SourceRow,
  type TemplateRow,
} from "./rows";
import {
  DEFAULT_MATERIALIZER_SETTINGS,
  LocalMaterializerSettingsStore,
  MATERIALIZER_SECTIONS,
  type MaterializerSettings,
  type MaterializerSettingsStore,
} from "./settings";
import { useCompositions, useRecipes, useTemplates } from "./useCompose";
import { FORMATS, formatWord } from "./words";

// THE MATERIALIZER: where a person and the model compose data from the
// memory graph into a file (epic memql#4977, design record
// docs/superpowers/specs/2026-09-05-compose-materializer-design.md).
//
// ===========================================================================
// THREE FEEDS AT THE ROOT, ONE PER CONCEPT
// ===========================================================================
// Compositions, templates and recipes are retained here and passed down,
// so the Composer's template picker and the Templates section cannot
// disagree about what the cluster holds. The rule is per CONCEPT rather
// than per app, so three feeds is not three copies of one thing.
//
// ===========================================================================
// THE SELECTION IS THE APP'S, NOT A SECTION'S
// ===========================================================================
// Following a composition from the list into the composer crosses
// sections, and a selection held inside a section would be lost on the
// way. So the id lives here and both sections are told what is open.

export function MaterializerApp({
  sectionId,
  navigate,
  askContext,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: MaterializerSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists
  // -- nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalMaterializerSettingsStore(), [store]);
  const [settings, setSettings] = useState<MaterializerSettings>(() => settingsStore.load());

  const compositions = useCompositions();
  const templates = useTemplates();
  const recipes = useRecipes();

  const materialize = useMaterialize();
  const compositionActs = useCompositionActs();
  const templateActs = useTemplateActs();
  const recipeActs = useRecipeActs();
  // THE SHELL, OR NULL. The only thing this app asks the shell for is
  // handing off to another one -- Files for the output, Nexus for the
  // goal -- and both happen on a click. Reaching for `useOs` here would
  // make the whole app unmountable without a desktop, which costs every
  // one of its tests a shell it does not otherwise need.
  const os = useOsIfPresent();

  const [openCompositionId, setOpenCompositionId] = useState("");

  const compositionRows = useMemo(
    () => compositions.snapshot.rows.filter((r) => compositionFromRow(r).id !== ""),
    [compositions.snapshot],
  );
  const templateRows: TemplateRow[] = useMemo(
    () => templates.snapshot.rows.map(templateFromRow).filter((t) => t.id !== ""),
    [templates.snapshot],
  );
  const recipeRows: RecipeRow[] = useMemo(
    () => recipes.snapshot.rows.map(recipeFromRow).filter((r) => r.id !== ""),
    [recipes.snapshot],
  );

  // The open composition is read out of the feed the list already holds
  // rather than through a second by-id read -- one source of truth for the
  // row that decides which acts the bar offers.
  const openRaw = useMemo(
    () => compositionRows.find((r) => idTail(compositionFromRow(r).id) === idTail(openCompositionId)) ?? null,
    [compositionRows, openCompositionId],
  );
  const open: CompositionRow | null = openRaw ? compositionFromRow(openRaw) : null;
  const openSources: SourceRow[] = openRaw ? sourcesOf(openRaw) : [];
  const openModels: ModelRow[] = openRaw ? modelsOf(openRaw) : [];

  function openComposition(id: string) {
    if (id.trim() === "") return;
    setOpenCompositionId(id);
    askContext(`materializer composition:${idTail(id)}`);
    navigate("composer");
  }

  // A STANDING OPEN INSTRUCTION, id-matched on consumption so acting on a
  // stale render can never eat a newer one. The payload names the one
  // thing this app can open; anything else is left alone rather than
  // guessed at, which keeps an unrelated opener from moving somebody's
  // window.
  const handled = useRef("");
  useEffect(() => {
    if (intent === undefined || intent.id === handled.current) return;
    const raw = intent.payload["compositionId"];
    const compositionId = typeof raw === "string" ? raw : "";
    if (compositionId === "") return;
    handled.current = intent.id;
    setOpenCompositionId(compositionId);
    // The section the opener asked for is respected: a Files row's "open
    // in Materializer" wants the composer, and a link into the list wants
    // the list. Defaulting to one would move somebody who said where they
    // wanted to be.
    navigate(sectionId === "materialized" ? "materialized" : "composer");
    consumeIntent?.(intent.id);
  }, [intent]);

  function update(patch: Partial<MaterializerSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- the pattern
  // every app since Fleet uses. The shell opens an app on its manifest's
  // FIRST section, so an app-level "open me here" can only be the app
  // navigating itself on first render, and it applies ONLY when the window
  // opened on the shell's default: a window opened on a named section was
  // opened by somebody who said where they wanted to be.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    const shellDefault = MATERIALIZER_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  if (sectionId === "settings") {
    return <MaterializerSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app={MATERIALIZER_APP_ID}
        subjectConcepts={MATERIALIZER_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "templates") {
    return (
      <TemplatesSection
        templates={templateRows}
        recipes={recipeRows}
        busy={templateActs.busy || recipeActs.busy}
        error={templateActs.error || recipeActs.error}
        showArchived={settings.showArchived}
        onCreateTemplate={(facts) => void templateActs.create(facts)}
        onArchiveTemplate={(id) => void templateActs.archive(id)}
        onRestoreTemplate={(id) => void templateActs.restore(id)}
        onRunRecipe={(id) => {
          void recipeActs.runRecipe(id, "").then((compositionId) => {
            if (compositionId) openComposition(compositionId);
          });
        }}
        onArchiveRecipe={(id) => void recipeActs.archive(id)}
      />
    );
  }
  if (sectionId === "materialized") {
    return (
      <MaterializedSection
        source={compositions.source}
        showArchived={settings.showArchived}
        selectedId={openCompositionId}
        onSelect={openComposition}
        error={compositionActs.error}
      />
    );
  }

  return (
    <ComposerSection
      templates={templateRows}
      composition={open}
      compositionSources={openSources}
      compositionModels={openModels}
      showUnmarkedConcepts={settings.showUnmarkedConcepts}
      defaultFormat={settings.defaultFormat}
      busy={materialize.busy || compositionActs.busy}
      error={materialize.error || compositionActs.error}
      onNewComposition={() => setOpenCompositionId("")}
      onMaterialize={(facts) => {
        void materialize
          .materialize({ ...facts, folderId: "", accountIds: [] })
          .then((id) => {
            if (id) setOpenCompositionId(id);
          });
      }}
      onAct={(act) => {
        if (open === null) return;
        switch (act) {
          case "stop":
            void compositionActs.stop(open.id);
            break;
          case "archive":
            void compositionActs.archive(open.id);
            break;
          case "restore":
            void compositionActs.restore(open.id);
            break;
          case "openFile":
            // THE FILE OPENS IN FILES, which is where a file lives. This
            // app never grows a file tree of its own -- that is the seam
            // agreed with the Files-places epic, and it runs one way.
            os?.actions.openApp("files", "browse", { artifactId: open.outputFileId });
            break;
          case "openGoal":
            os?.actions.openApp(GOAL_APP_ID, GOAL_APP_SECTION, goalIntent(open.goalId));
            break;
          case "startOver":
            setOpenCompositionId("");
            break;
          case "saveRecipe":
            void compositionActs.saveRecipe({
              name: open.name,
              description: open.statement,
              format: open.format,
              templateId: open.templateId,
              folderId: open.folderId,
              // THE SELECTORS ARE THE SOURCES THAT WERE QUERIES. A
              // concept_row source names one row that existed at that
              // instant, and carrying it into a recipe would make "run it
              // again" mean "make another copy of that same row" -- which
              // is the opposite of what a recipe is for.
              sourceSelectors: openSources
                .filter((s) => s.kind === "query")
                .map((s) => ({ kind: "concept_query", selector: s.ref, label: s.label })),
            });
            break;
        }
      }}
    />
  );
}

/** The tail of a canonical id, for comparing a bare id against a stored one. */
function idTail(id: string): string {
  const trimmed = id.trim();
  const i = trimmed.lastIndexOf(":");
  return i >= 0 ? trimmed.slice(i + 1) : trimmed;
}

function MaterializerSettingsSection({
  settings,
  update,
}: {
  settings: MaterializerSettings;
  update: (patch: Partial<MaterializerSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Materializer settings" />
      <Panel label="Materializer settings">
        <fieldset className="os-field-group">
          <legend>Open the Materializer on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {MATERIALIZER_SECTIONS.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Applies the next time a Materializer window opens; it does not move the window you are
            looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Start on</legend>
          <Select
            id="mz-default-format"
            value={settings.defaultFormat}
            onChange={(defaultFormat) => update({ defaultFormat })}
            label="Kind of file to start on"
          >
            {FORMATS.map((f) => (
              <option key={f} value={f}>
                {formatWord(f)}
              </option>
            ))}
          </Select>
          <p className="os-caption">
            The kind of file the Target column starts on. Change it per composition whenever you
            like — this only decides where it begins.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Archived</legend>
          <Check
            checked={settings.showArchived}
            onChange={(showArchived) => update({ showArchived })}
          >
            List archived compositions, templates and recipes
          </Check>
          <p className="os-caption">
            Off by default: archiving something is how you say you are done with it. Archiving a
            record never touches the file it names — the file is an ordinary Library row with its
            own place in the Bin.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>What you can compose from</legend>
          <Check
            checked={settings.showUnmarkedConcepts}
            onChange={(showUnmarkedConcepts) => update({ showUnmarkedConcepts })}
          >
            Offer every concept, not only the marked ones
          </Check>
          <p className="os-caption">
            A concept is marked in the DSL as worth composing from, and the marked ones are what
            Sources offers. Unmarked is not forbidden — turning this on lists the rest after them,
            saying which they are.
          </p>
        </fieldset>

        {/* THE ABSENT CONTROL, WITH AN ACCOUNT OF ITSELF. Somebody who
            opens settings in an app that spends model calls looks for a
            budget field. There is none, and an absent control with
            nothing said about it reads as something nobody got round to
            building. */}
        <fieldset className="os-field-group">
          <legend>Spending</legend>
          <p className="os-caption">
            Not set here. A materialization is a goal, and a goal's ceilings — tokens, cost, wall
            clock, retries — are set when the goal is accepted. One step of a materialization
            reaches a model at all, and a repeat of one already done reaches none.
          </p>
        </fieldset>
      </Panel>
      <Caption>
        These are kept in this browser, separately from your desktop, so an app learning a checkbox
        can never cost you your desks. The defaults are{" "}
        {DEFAULT_MATERIALIZER_SETTINGS.defaultSection} with archived hidden.
      </Caption>
    </div>
  );
}
