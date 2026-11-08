import { useState, type ReactNode } from "react";
import { FilesApp } from "../src/apps/files/FilesApp";
import { MaterializerApp } from "../src/apps/materializer/MaterializerApp";
import { LocalFilesSettingsStore } from "../src/apps/files/settings";
import { fakeConnection as filesConnection, artifactRow, folderRow, withSession as filesSession } from "../test/files/harness";
import { fakeConnection as composeConnection, compositionRow, templateRow, recipeRow, memoryStore, withSession as composeSession } from "../test/materializer/harness";
import { OsProvider } from "../src/chrome/state";
import { OS_REGISTRY } from "../src/apps/registry";
import { MachinesProvider } from "../src/live/machines";

const noop = () => {};
const dates = (index: number) => `2026-10-${String(index + 1).padStart(2, "0")}T12:00:00Z`;
function Frame({ children }: { children: ReactNode }) {
  return <OsProvider registry={OS_REGISTRY} actorRole="owner" grid={{cols:8,rows:5}}><MachinesProvider><div className="os-window-content" style={{ height: "100dvh" }}>{children}</div></MachinesProvider></OsProvider>;
}
function ComposeScene({ section }: { section: string }) {
  const [current, setCurrent] = useState(section);
  const [store] = useState(() => memoryStore());
  return <MaterializerApp sectionId={current} navigate={setCurrent} askContext={noop} store={store} />;
}
export const RESOURCE_PAGE_VIEWS = {
  "files-paged": {
    connect: () => filesConnection({ artifacts: Array.from({length:25}, (_, n) => artifactRow({ id:`artifact-${n}`, title:n === 0 ? "Ancient needle.pdf" : `Document ${n}.pdf`, folderId:"folder", sourceConceptRef:`file-${n}`, createdAt:dates(n) })), folders:[folderRow({id:"folder", name:"Documents"})] }),
    wrap: (el: ReactNode) => filesSession(<Frame>{el}</Frame>),
    render: () => <FilesApp sectionId="browse" navigate={noop} askContext={noop} store={new LocalFilesSettingsStore(null)} />,
  },
  ...Object.fromEntries(["materialized", "templates", "recipes"].map(section => [`materializer-${section}-paged`, {
    connect: () => composeConnection({
      compositions: Array.from({length:25}, (_, n) => compositionRow({id:`composition-${n}`, name:n===0?"Ancient needle":"Quarterly report "+n, createdAt:dates(n)})),
      templates: Array.from({length:25}, (_, n) => templateRow({id:`template-${n}`, name:n===0?"Ancient needle":"Template "+n, createdAt:dates(n)})),
      recipes: Array.from({length:25}, (_, n) => recipeRow({id:`recipe-${n}`, name:n===0?"Ancient needle":"Recipe "+n, createdAt:dates(n)})),
    }),
    wrap: (el: ReactNode) => composeSession(<Frame>{el}</Frame>),
    render: () => <ComposeScene section={section} />,
  }])),
};
