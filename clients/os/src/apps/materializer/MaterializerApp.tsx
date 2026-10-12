import "./materializer.css";
import { useEffect, useMemo, useRef, useState } from "react";
import { refusalSentence, rowNumber, rowString, sameEntityId } from "@znasllc-io/memql-sdk-core/client";
import { useAuthSource } from "../../auth/context";
import { useOsConnection } from "../../live/connection";
import { COMPOSE_FORMATS, MATERIALIZER_COMPOSE_SECTION, readComposeIntent, type ComposeSetup } from "../../items/materialize";
import { openCheckedArtifact } from "../../items/openFile";
import { readEditorPreference } from "../../items/editorPreference";
import { browserHandoffPorts, VSCODE_NO_ANSWER_MESSAGE, type HandoffPorts } from "../../items/vscode";
import { folderOptions } from "../../items/FolderSelect";
import { downloadArtifact } from "../files/actions/download";
import { folderFromRow } from "../files/rows";
import { useAccountOptions } from "../accounts/tie";
import { useDefaultOrganization } from "../accounts/organization";
import { accountName } from "../accounts/rows";
import { ContentSkeleton, Notice, listCount, moduleActFor, useReach } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import { useOsIfPresent } from "../../chrome/state";
import { appById, canOpen, type OsAppProps } from "../../system/registry";
import { useSession } from "../../chrome/access";
import { MaterializedSection } from "./MaterializedSection";
import { SettingsSection } from "./SettingsSection";
import { RecipesSection } from "./RecipesSection";
import { TemplatesSection } from "./TemplatesSection";
import { CompositionPage } from "./CompositionPage";
import { MakeWizard } from "./MakeWizard";
import { pageActs, type ActId } from "./acts";
import { MATERIALIZER_APP_ID, MATERIALIZER_LOG_CONCEPTS } from "./concepts";
import { DEPLOY_APP_ID, DEPLOY_APP_SECTION, GOAL_APP_ID, GOAL_APP_SECTION, goalIntent } from "./handoff";
import { useCompositionActs, useMaterialize, useRecipeActs, useTemplateActs, recipeFacts } from "./actions";
import { useDescriptionHandOff, useFilesEntries } from "./descriptionHandOff";
import { reserveEditor, useEditorOpening } from "./editorOpening";
import { compositionFromRow, modelsOf, recipeKeeps, sourcesOf, templateFromRow } from "./rows";
import { LocalMaterializerSettingsStore, MATERIALIZER_SECTIONS, type MaterializerSettings, type MaterializerSettingsStore } from "./settings";
import { useCompositions, useRecipes, useTemplates, useFolders, useLibraryFiles, useComposableConcepts, useResolvedSources } from "./useCompose";
import { isRunning, madeCaption } from "./words";
import { draftFromComposition, draftFromSetup, keepUsableTemplate, openingStep, pathOf, requestOf, type MakeDraft, type MakeFacts, type MakeStepId } from "./make";
import { fileLookup, fileOptions } from "./ingredientFiles";

export function MaterializerApp({ sectionId, navigate, askContext, intent, consumeIntent, store, handoffPorts = browserHandoffPorts }: OsAppProps & { store?: MaterializerSettingsStore; handoffPorts?: HandoffPorts }) {
  const settingsStore = useMemo(() => store ?? new LocalMaterializerSettingsStore(), [store]);
  const [settings, setSettings] = useState<MaterializerSettings>(() => settingsStore.load());
  const defaultFormat = COMPOSE_FORMATS.find(format => format === settings.defaultFormat) ?? "markdown";
  const compositions = useCompositions(), templates = useTemplates(), recipes = useRecipes();
  const accounts = useAccountOptions(), defaultAccount = useDefaultOrganization(accounts);
  const materialize = useMaterialize(), compositionActs = useCompositionActs(), templateActs = useTemplateActs(), recipeActs = useRecipeActs();
  const os = useOsIfPresent(), reach = useReach();
  const { config, readiness } = useSession();
  const connection = useOsConnection(), authSource = useAuthSource();
  const [openId, setOpenId] = useState("");
  const [draft, setDraft] = useState<MakeDraft>(() => draftFromSetup(null, defaultFormat));
  const [step, setStep] = useState<MakeStepId>("from");
  const [making, setMaking] = useState<{ id: string; since: number } | null>(null);
  const [returnTo, setReturnTo] = useState<ComposeSetup["returnTo"]>();
  const [returnFor, setReturnFor] = useState<{ id: string; to: NonNullable<ComposeSetup["returnTo"]> } | null>(null);
  const [madeHere, setMadeHere] = useState<string[]>([]);
  const [stopped, setStopped] = useState<ReadonlySet<string>>(new Set());
  const [saved, setSaved] = useState<ReadonlySet<string>>(new Set());
  const [tabRefusals, setTabRefusals] = useState<Record<string, string>>({});
  const [openError, setOpenError] = useState("");
  const generation = useRef(0), starting = useRef(false);
  const currentSection = useRef(sectionId); currentSection.current = sectionId;
  const cancelHandoff = useRef<(() => void) | null>(null);
  useEffect(() => () => cancelHandoff.current?.(), []);
  const [templateFilesNeeded, setTemplateFilesNeeded] = useState(false);
  const libraryFiles = useLibraryFiles(sectionId === "composer" || templateFilesNeeded);
  const folders = useFolders(sectionId === "composer" || !!openId || sectionId === "recipes");
  const folderRows = useMemo(() => folders.snapshot.rows.map(folderFromRow), [folders.snapshot]);
  const templateRows = useMemo(() => templates.snapshot.rows.map(templateFromRow), [templates.snapshot]);
  const verdict = readiness?.loaded ? readiness.of("ai") : null;
  const facts: MakeFacts = { templates: templateRows, templatesLoaded: listCount(templates.snapshot) !== undefined,
    inference: !verdict || verdict.state === "unknown" ? null : verdict.state === "unconfigured" ? false : true,
    files: fileLookup(libraryFiles.snapshot.rows) };
  const inferenceAct = moduleActFor({ id: "ai", verdict, sectionsOf: reach.sectionsOf, canOpenWindows: reach.canOpenWindows });
  const records = useComposableConcepts(settings.showUnmarkedConcepts, sectionId === "composer");
  const resolved = useResolvedSources(sectionId === "composer" ? draft.ingredients : []);
  const openRaw = compositions.snapshot.rows.find(row => sameEntityId(rowString(row,"id"), openId));
  const open = openRaw ? compositionFromRow(openRaw) : null;
  const sources = openRaw ? sourcesOf(openRaw) : [], models = openRaw ? modelsOf(openRaw) : [];
  const entryFileIds = compositions.snapshot.rows.filter(row => madeHere.some(id => sameEntityId(id, rowString(row,"id"))))
    .map(row => rowString(row,"outputFileId")).filter(Boolean);
  const entries = useFilesEntries(entryFileIds);
  const descriptions = useDescriptionHandOff(materialize.described, compositions.snapshot.rows, stopped, entries);
  const editor = useEditorOpening({ domain: config.domain, compositions: compositions.snapshot.rows, entries, outcomes: descriptions, described: materialize.described });
  const description = descriptions.find(outcome => sameEntityId(outcome.compositionId, openId));
  const describedHere = materialize.described.some(file => sameEntityId(file.compositionId, openId));
  const backTo = open && returnFor && sameEntityId(open.id, returnFor.id) && os && canOpen(os.registry, returnFor.to.appId) ? appById(os.registry, returnFor.to.appId)?.name : undefined;
  const folderName = (id: string): string | null | undefined => !id ? "Library" : folderRows.find(folder => sameEntityId(folder.id, id))?.name ?? (listCount(folders.snapshot) === undefined ? undefined : null);
  const names = { folder: folderName, template: (id: string) => templateRows.find(t => sameEntityId(t.id,id))?.name || "Unavailable template", account: (id: string) => { const account = accounts.find(a => sameEntityId(a.id,id)); return account ? accountName(account) : "Unavailable organization"; } };

  function begin(next: MakeDraft, to?: ComposeSetup["returnTo"]) {
    generation.current++; materialize.reset(); compositionActs.reset(); setOpenError("");
    const usable = keepUsableTemplate(next, facts);
    setDraft(usable); setStep(openingStep(usable, facts)); setMaking(null); setOpenId(""); setReturnFor(null); setReturnTo(to);
    navigate(MATERIALIZER_COMPOSE_SECTION);
  }
  function beginSetup(setup: ComposeSetup | null) {
    const next = draftFromSetup(setup, defaultFormat);
    if (next.fileKind.kind === "emailTemplate") next.accountId = defaultAccount;
    begin(next, setup?.returnTo);
  }
  useEffect(() => { setDraft(previous => keepUsableTemplate(previous, facts)); }, [templates.snapshot, draft.fileKind]);
  useEffect(() => {
    if (draft.fileKind.kind === "emailTemplate" && !draft.accountId && defaultAccount) setDraft(previous => ({ ...previous, accountId: defaultAccount }));
  }, [defaultAccount]);
  const previousSection = useRef(sectionId);
  useEffect(() => {
    if (previousSection.current === "composer" && sectionId !== "composer") {
      generation.current++;
      setDraft(draftFromSetup(null,defaultFormat)); setStep("from"); setReturnTo(undefined); setMaking(null);
    }
    previousSection.current = sectionId;
  }, [sectionId]);
  function showComposition(id: string) {
    setOpenId(id); setOpenError(""); compositionActs.reset(); askContext(`materializer composition:${id}`);
    const row = compositions.snapshot.rows.find(row => sameEntityId(rowString(row,"id"),id));
    const c = row ? compositionFromRow(row) : null;
    if (c && isRunning(c.status)) {
      setDraft(draftFromComposition(c, sourcesOf(row!))); setMaking({ id, since: Date.parse(c.createdAt) || Date.now() }); navigate("composer");
    } else { setMaking(null); navigate("materialized"); }
  }
  useEffect(() => {
    if (sectionId !== "materialized" || !open || !isRunning(open.status)) return;
    setDraft(draftFromComposition(open, sources));
    setMaking({id:open.id,since:Date.parse(open.createdAt)||Date.now()});
    navigate("composer");
  }, [sectionId, open?.id, open?.status]);
  const handled = useRef("");
  useEffect(() => {
    if (!intent || handled.current === intent.id) return;
    const instruction = instructionFor(intent.payload); if (!instruction) return;
    handled.current = intent.id;
    if ("setup" in instruction) beginSetup(instruction.setup); else showComposition(instruction.compositionId);
    consumeIntent?.(intent.id);
  }, [intent]);
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return; applied.current = true;
    if (intent && instructionFor(intent.payload)) return;
    if (sectionId === MATERIALIZER_SECTIONS[0]?.id && settings.defaultSection !== sectionId) navigate(settings.defaultSection);
  }, []);
  useEffect(() => {
    if (sectionId !== "composer" || !making) return;
    const row = compositions.snapshot.rows.find(row => sameEntityId(rowString(row,"id"),making.id));
    if (!row) return;
    const c = compositionFromRow(row);
    if (!isRunning(c.status)) { setOpenId(c.id); setMaking(null); navigate("materialized"); }
  }, [compositions.snapshot, making, sectionId]);

  function leave() {
    generation.current++; setMaking(null); setDraft(draftFromSetup(null,defaultFormat)); setOpenId(""); materialize.reset();
    const to = returnTo ?? (making && returnFor?.id === making.id ? returnFor.to : undefined); setReturnTo(undefined); navigate("materialized");
    if (to && os && canOpen(os.registry,to.appId)) os.actions.openApp(to.appId,to.sectionId);
  }
  function make() {
    if (starting.current || making) return;
    starting.current = true;
    const heldGeneration = generation.current, to = returnTo, since = Date.now();
    const reserved = reserveEditor(pathOf(draft,facts),handoffPorts,readEditorPreference());
    void materialize.start(requestOf(draft),facts.files).then(id => {
      if (!id) { reserved.reservation?.close(); return; }
      setMadeHere(previous => [...previous,id]);
      if (reserved.reservation) editor.hold(id,reserved.reservation);
      if (reserved.refusal) setTabRefusals(previous => ({ ...previous,[id]: reserved.refusal }));
      if (generation.current !== heldGeneration || currentSection.current !== "composer") return;
      setMaking({ id,since }); setOpenId(id); setReturnFor(to ? { id,to } : null); setReturnTo(undefined);
    }).finally(() => { starting.current = false; });
  }
  function stop() {
    if (!making) return;
    const id = making.id;
    editor.release(id);
    setStopped(previous => new Set([...previous,id]));
    void compositionActs.stop(id).then(result => {
      if (result === "refused") setStopped(previous => { const next = new Set(previous); next.delete(id); return next; });
    });
  }
  function openFile(fileId: string, title: string) {
    cancelHandoff.current?.(); setOpenError("");
    void openCheckedArtifact({ domain: config.domain,title,ports: handoffPorts,onNoAnswer: () => setOpenError(VSCODE_NO_ANSWER_MESSAGE),
      load: async () => {
        if (!connection) throw new Error("Connect to the cluster before opening this file.");
        const [artifactResult,fileResult] = await Promise.all([connection.query.libraryArtifactForFile({ fileId }),connection.query.libraryFileById({ fileId })]);
        const artifact = artifactResult.rows()[0], file = fileResult.rows()[0];
        if (!artifact || !file) throw new Error("This file is not available in your Library yet. Try opening it again.");
        return { artifactId: rowString(artifact,"id"),name: rowString(file,"name") || title,mimeType: rowString(file,"mimeType"),fileId,sizeBytes: rowNumber(file,"size") };
      },download: file => downloadArtifact({ artifactId: file.artifactId!,name: file.name,fileId,readFile: async () => file,bearer: () => authSource.bearer() }),
    }).then(cancel => { cancelHandoff.current = cancel; }).catch(error => setOpenError(refusalSentence(error)));
  }
  function act(id: ActId) {
    if (!open) return;
    if (id === "archive") void compositionActs.archive(open.id);
    if (id === "restore") void compositionActs.restore(open.id);
    if (id === "openFile") openFile(open.outputFileId,open.name);
    if (id === "openDeployables") os?.actions.openApp(DEPLOY_APP_ID,DEPLOY_APP_SECTION,{});
    if (id === "returnToApp" && returnFor) os?.actions.openApp(returnFor.to.appId,returnFor.to.sectionId);
    if (id === "startOver") begin(draftFromComposition(open,sources));
    if (id === "saveRecipe") void compositionActs.saveRecipe(recipeFacts(open,sources)).then(recipe => { if (recipe) setSaved(previous => new Set([...previous,open.id])); });
  }
  function update(patch: Partial<MaterializerSettings>) { const next = { ...settings,...patch,version: 1 as const }; setSettings(next); settingsStore.save(next); }
  const makingRaw = making ? compositions.snapshot.rows.find(row => sameEntityId(rowString(row,"id"),making.id)) : undefined;
  const run = makingRaw ? compositions.runOf(rowString(makingRaw,"runId")) : undefined;
  const runStart = run ? Date.parse(rowString(run,"startedAt")) : NaN;
  return <>
    <div hidden={sectionId !== "materialized" || !!openId} className="mz-list-host">
      <MaterializedSection onRetry={compositions.reseed} source={compositions.source} showArchived={settings.showArchived} selectedId={openId} onSelect={showComposition}
        error={sectionId === "materialized" && !openId ? compositionActs.error : ""} onMake={() => beginSetup(null)} navigation={sectionId === "materialized" && !openId} />
    </div>
    {sectionId === "materialized" && openId ? open ? <CompositionPage composition={open} sources={sources} models={models}
      filedIn={folderName(open.folderId) ?? (listCount(folders.snapshot) !== undefined ? "Unavailable folder" : undefined)}
      templateName={names.template(open.templateId)} organizationName={open.accountIds.map(names.account).join(", ")}
      caption={madeCaption(open,sources,models,describedHere,description)} description={description} recipeSaved={saved.has(open.id)} editorSilent={openError} tabRefusal={tabRefusals[openId]}
      acts={pageActs(open,{ backTo,recipeCanRun: !!open.statement.trim() || sources.some(recipeKeeps),recipeSaved: saved.has(open.id) })}
      busy={compositionActs.busy} error={compositionActs.error} canOpenRun={!!open.goalId && !!os && canOpen(os.registry,GOAL_APP_ID)}
      onAct={act} onOpenRun={() => os?.actions.openApp(GOAL_APP_ID,GOAL_APP_SECTION,goalIntent(open.goalId))} onBack={() => setOpenId("")} />
      : listCount(compositions.snapshot) === undefined ? <ContentSkeleton label="Reading the file record" /> : <Notice tone="info" sentence="That file record is not available." /> : null}
    {sectionId === "composer" ? <MakeWizard draft={draft} onDraft={setDraft} facts={facts} names={names} open={step} onOpen={setStep}
      ingredients={{ files: fileOptions(libraryFiles.snapshot.rows),filesLoading: listCount(libraryFiles.snapshot) === undefined,filesError: libraryFiles.snapshot.error || "",retryFiles: libraryFiles.reseed,records,resolved }}
      folders={folderOptions(folderRows)} foldersLoaded={listCount(folders.snapshot) !== undefined} foldersError={folders.snapshot.error || ""} accounts={accounts}
      refusal={materialize.error || compositionActs.error} starting={materialize.busy} making={making ? { since: Number.isFinite(runStart) ? runStart : making.since,
        detail: rowString(makingRaw ?? {},"status") === "composing" ? "Composing" : rowString(makingRaw ?? {},"status") === "rendering" ? "Writing the file" : "Starting" } : undefined}
      canSetUpInference={inferenceAct.kind === "open"} inferenceElsewhere="An owner or developer can set it up."
      onMake={make} onCancel={leave} onStop={stop} onLeave={leave} onSetUpInference={() => { if (inferenceAct.kind === "open") reach.open(inferenceAct.app,inferenceAct.section); }} /> : null}
    {sectionId === "templates" ? <TemplatesSection source={templates.source} accounts={accounts} files={libraryFiles.snapshot.rows}
      filesLoading={listCount(libraryFiles.snapshot) === undefined} filesError={libraryFiles.snapshot.error || ""}
      busy={templateActs.busy} error={templateActs.error} showArchived={settings.showArchived} onCreateTemplate={templateActs.create}
      onFilesNeeded={setTemplateFilesNeeded} onRetry={templates.reseed} onReset={templateActs.reset}
      onArchiveTemplate={id => void templateActs.archive(id)} onRestoreTemplate={id => void templateActs.restore(id)} /> : null}
    {sectionId === "recipes" ? <RecipesSection source={recipes.source} showArchived={settings.showArchived} busy={recipeActs.busy} error={recipeActs.error}
      names={names} onRetry={recipes.reseed} onReset={recipeActs.reset} onArchive={id=>void recipeActs.archive(id)} onRestore={id=>void recipeActs.restore(id)}
      onRun={recipe=>{void recipeActs.runRecipe(recipe.id,"").then(id=>{if(!id)return; setDraft({...draftFromSetup({format:COMPOSE_FORMATS.find(f=>f===recipe.format)||"markdown",folderId:recipe.folderId},defaultFormat),name:recipe.name,statement:recipe.description});setMaking({id,since:Date.now()});setOpenId(id);navigate("composer");});}}/>:null}
    {sectionId === "settings" ? <SettingsSection settings={settings} update={update} /> : null}
    {sectionId === "logs" ? <AppLogsSection app={MATERIALIZER_APP_ID} subjectConcepts={MATERIALIZER_LOG_CONCEPTS} intent={intent} consumeIntent={consumeIntent} /> : null}
  </>;
}

function instructionFor(payload: Record<string,unknown>): { compositionId: string } | { setup: ComposeSetup } | null {
  if (typeof payload.compositionId === "string" && payload.compositionId.trim()) return { compositionId: payload.compositionId };
  const setup = readComposeIntent(payload); return setup ? { setup } : null;
}
