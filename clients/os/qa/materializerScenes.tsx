import { useEffect, useState } from "react";
import { MaterializerApp } from "../src/apps/materializer/MaterializerApp";
import { MakeWizard } from "../src/apps/materializer/MakeWizard";
import { CompositionPage } from "../src/apps/materializer/CompositionPage";
import { pageActs } from "../src/apps/materializer/acts";
import {
  draftFromSetup,
  type MakeDraft,
  type MakeStepId,
} from "../src/apps/materializer/make";
import {
  fileLookup,
  fileOptions,
} from "../src/apps/materializer/ingredientFiles";
import {
  compositionFromRow,
  modelsOf,
  sourcesOf,
} from "../src/apps/materializer/rows";
import {
  fakeConnection,
  compositionRow,
  libraryFileRow,
  folderRow,
  templateRow,
  recipeRow,
  memoryStore,
  withSession,
  type FakeSeed,
} from "../test/materializer/harness";
import { OsProvider } from "../src/chrome/state";
import { OS_REGISTRY } from "../src/apps/registry";
const noop = () => {};
const files = [
  libraryFileRow({ id: "notes", name: "Workshop notes.md" }),
  libraryFileRow({
    id: "figures",
    name: "Quarterly results.csv",
    mimeType: "text/csv",
  }),
  libraryFileRow({
    id: "cover",
    name: "Report shell.docx",
    mimeType:
      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
  }),
];
const seed: FakeSeed = {
  compositions: [
    compositionRow({
      id: "report",
      name: "Quarterly report",
      folderId: "reports",
    }),
    compositionRow({
      id: "notes-made",
      name: "Workshop notes",
      format: "markdown",
      modelsUsed: [],
      sources: [],
      provenanceEmbedded: false,
      provenanceNote: "Created blank.",
    }),
  ],
  libraryFiles: files,
  folders: [folderRow({ id: "reports", name: "Reports" })],
  templates: [
    templateRow({
      id: "shell",
      name: "Quarterly report",
      fileId: "cover",
      placeholders: [
        {
          name: "Quarter",
          description: "The reporting quarter",
          required: true,
        },
      ],
    }),
  ],
  recipes: [
    recipeRow({
      id: "recipe",
      name: "Quarterly report",
      runCount: 3,
      lastRunAt: "2026-10-02T09:00:00Z",
    }),
  ],
  composables: {
    concepts: [
      {
        id: "v1:x:invoice",
        as: "Open invoices",
        list: "openInvoices",
        fields: [],
        marked: true,
      },
    ],
    registryAvailable: true,
  },
};
function AppScene({ section, press }: { section: string; press?: string }) {
  const [current, setCurrent] = useState(section);
  useEffect(() => {
    if (!press) return;
    const t = setTimeout(() => {
      Array.from(document.querySelectorAll<HTMLButtonElement>("button"))
        .find(
          (b) =>
            !b.closest("[hidden]") &&
            (b.getAttribute("aria-label") === press ||
              b.textContent?.trim().startsWith(press)),
        )
        ?.click();
    }, 800);
    return () => clearTimeout(t);
  }, [press]);
  return (
    <MaterializerApp
      sectionId={current}
      navigate={setCurrent}
      askContext={noop}
      store={memoryStore()}
    />
  );
}
function WizardScene({ variant }: { variant: string }) {
  const initial = draftFromSetup(
    variant === "from-files"
      ? { format: "markdown", folderId: "reports" }
      : null,
    "markdown",
  );
  if (["mixed", "ready", "inference", "making"].includes(variant)) {
    initial.name = "Quarterly summary";
    initial.statement =
      "Summarize the results for our board, with the main decisions first.";
  }
  if (variant === "mixed")
    initial.ingredients = [
      { kind: "library_file", ref: "notes", label: "Workshop notes.md" },
      { kind: "library_file", ref: "figures", label: "Quarterly results.csv" },
      { kind: "query", ref: "query openInvoices()", label: "Open invoices" },
    ];
  if (variant === "blank-pdf") {
    initial.name = "Quarterly summary";
    initial.fileKind = { kind: "document", format: "pdf" };
  }
  if (variant === "refused") {
    initial.name = "Quarterly summary";
    initial.folderId = "gone";
  }
  const [draft, setDraft] = useState<MakeDraft>(initial),
    [open, setOpen] = useState<MakeStepId>(
      variant === "from-files"
        ? "name"
        : variant === "refused"
          ? "folder"
          : variant === "ready" || variant === "inference"
            ? "describe"
            : "from",
    );
  return (
    <MakeWizard
      draft={draft}
      onDraft={setDraft}
      facts={{
        templates: [],
        templatesLoaded: true,
        inference: variant === "inference" ? false : true,
        files: fileLookup(files),
      }}
      names={{
        folder: (id) => (id === "reports" ? "Reports" : id ? null : "Library"),
        template: () => "Quarterly report",
        account: () => "Example company",
      }}
      open={open}
      onOpen={setOpen}
      ingredients={{
        files: fileOptions(files),
        filesLoading: false,
        filesError: "",
        retryFiles: noop,
        records: {
          concepts: [
            {
              id: "v1:x:invoice",
              as: "Open invoices",
              list: "openInvoices",
              fields: [],
              description: "Unpaid invoices",
              marked: true,
            },
          ],
          loading: false,
          error: "",
          readAt: "2026-10-12",
          registryAvailable: true,
          reread: noop,
        },
        resolved: { sources: [], total: 0, loading: false, error: "" },
      }}
      folders={[{ id: "reports", label: "Reports" }]}
      foldersLoaded
      foldersError=""
      accounts={[]}
      refusal={
        variant === "refused"
          ? "That folder isn't available. Choose another folder."
          : ""
      }
      starting={false}
      making={
        variant === "making"
          ? { detail: "Composing", since: Date.now() - 12000 }
          : undefined
      }
      canSetUpInference
      onMake={noop}
      onCancel={noop}
      onStop={noop}
      onLeave={noop}
      onSetUpInference={noop}
    />
  );
}
function FileScene({ variant }: { variant: string }) {
  const raw = compositionRow({
    id: "report",
    name: "Quarterly report",
    status: variant === "failed" ? "failed" : "ready",
    failureReason:
      variant === "failed" ? "The source file is no longer available." : "",
    archived: variant === "archived",
    deployableKind: variant === "package" ? "spa" : "",
    format: variant === "package" ? "zip" : "pdf",
  });
  const c = compositionFromRow(raw);
  return (
    <CompositionPage
      composition={c}
      sources={sourcesOf(raw)}
      models={modelsOf(raw)}
      filedIn="Reports"
      templateName="Quarterly report"
      organizationName="Example company"
      caption={variant === "failed" ? "" : "The composed text is in the file."}
      recipeSaved={false}
      description={
        variant === "described"
          ? {
              failure: "The connection was interrupted.",
              unattached: ["Workshop notes.md"],
            }
          : undefined
      }
      acts={pageActs(c, {
        recipeCanRun: true,
        recipeSaved: false,
        backTo: variant === "trip" ? "Files" : undefined,
      })}
      busy={false}
      error=""
      canOpenRun
      onAct={noop}
      onOpenRun={noop}
      onBack={noop}
    />
  );
}
const wrap = (el: React.JSX.Element) =>
  withSession(
    <OsProvider
      registry={OS_REGISTRY}
      actorRole="owner"
      grid={{ cols: 12, rows: 8 }}
    >
      <div className="os-window-content" style={{ height: "100dvh" }}>
        {el}
      </div>
    </OsProvider>,
  );
const view = (render: () => React.JSX.Element, s: FakeSeed = seed) => ({
  connect: () => fakeConnection(s),
  wrap,
  render,
});
export const MATERIALIZER_VIEWS: Record<string, ReturnType<typeof view>> = {
  "materializer-list": view(() => <AppScene section="materialized" />),
  "materializer-list-empty": view(
    () => <AppScene section="materialized" />,
    {},
  ),
  "materializer-list-hidden": view(() => <AppScene section="materialized" />, {
    compositions: [compositionRow({ archived: true })],
  }),
  "materializer-templates": view(() => <AppScene section="templates" />),
  "materializer-templates-empty": view(
    () => <AppScene section="templates" />,
    {},
  ),
  "materializer-template-add": view(() => (
    <AppScene section="templates" press="Add a template" />
  )),
  "materializer-template": view(() => (
    <AppScene section="templates" press="Quarterly report" />
  )),
  "materializer-recipes": view(() => <AppScene section="recipes" />),
  "materializer-recipes-empty": view(() => <AppScene section="recipes" />, {}),
  "materializer-recipe": view(() => (
    <AppScene section="recipes" press="Quarterly report" />
  )),
  "materializer-settings": view(() => <AppScene section="settings" />),
  ...Object.fromEntries(
    [
      "plus",
      "mixed",
      "from-files",
      "ready",
      "blank-pdf",
      "inference",
      "refused",
    ].map((variant) => [
      `materializer-make-${variant}`,
      view(() => <WizardScene variant={variant} />),
    ]),
  ),
  "materializer-making": view(() => <WizardScene variant="making" />),
  ...Object.fromEntries(
    ["ready", "trip", "described", "failed", "package", "archived"].map(
      (variant) => [
        `materializer-file-${variant}`,
        view(() => <FileScene variant={variant} />),
      ],
    ),
  ),
};
