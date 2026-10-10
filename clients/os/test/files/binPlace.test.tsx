import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ OsConnectionProvider: ({children}: {children: React.ReactNode}) => children, useOsConnection: () => h.connection }));
import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";
beforeEach(() => { h.connection = fakeConnection(); });
describe("Bin workspace", () => {
 it("shows deleted files and folders once, and opens their details in place", async () => {
  h.connection = fakeConnection({ archivedFolders: [folderRow({id:"folder",name:"Old reports",archived:true})], artifacts: [artifactRow({id:"old",title:"old.pdf",folderId:"folder",archived:true}), artifactRow({id:"live",title:"live.pdf"})] });
  await renderFiles(); await click(screen.getByRole("button",{name:"Bin"}));
  expect(screen.queryByRole("button",{name:/live.pdf/})).toBeNull();
  await click(screen.getByRole("button",{name:"Open folder Old reports"}));
  await click(screen.getByRole("button",{name:/old.pdf/}));
  const detail = screen.getByRole("region",{name:"File details"});
  expect(within(detail).getByRole("button",{name:"Restore"})).toBeTruthy();
  expect(screen.queryByRole("navigation",{name:"File locations"})).toBeNull();
 });
 it("keeps empty archived folders reachable for restore", async () => {
  const connection = fakeConnection({archivedFolders:[folderRow({id:"empty",name:"Empty folder",archived:true})]}); h.connection=connection;
  await renderFiles(); await click(screen.getByRole("button",{name:"Bin"}));
  fireEvent.contextMenu(screen.getByRole("button",{name:"Open folder Empty folder"}));
  await click(screen.getByRole("menuitem",{name:"Restore"}));
  expect(connection.callsNamed("restoreLibraryFolder")).toEqual(['mutation restoreLibraryFolder(folderId: "empty")']);
 });
 it("shows an honest empty state",async()=>{
  await renderFiles(); await click(screen.getByRole("button",{name:"Bin"}));
  expect(screen.getByText(/The Bin is empty/)).toBeTruthy();
 });
});
