import {readTheBar} from "../readTheBar";
import { useState } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { Concepts, type Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, bridgePathFor: (base: string) => base + "_memql/ws", osBridgePath: "/_memql/ws" }));
const { MaterializerApp } = await import("../../src/apps/materializer/MaterializerApp");
const { composeIntent } = await import("../../src/items/materialize");
const { compositionRow, fakeConnection, libraryFileRow, memoryStore, refused, rowsResult, withSession } = await import("./harness");
const CONCEPT = "v1:compose:composition";
const ready = (over: Record<string, unknown> = {}) => compositionRow({ id: "made", name: "Notes", format: "markdown", statement: "", sources: [], modelsUsed: [], provenanceEmbedded: false, provenanceNote: "Created blank.", outputFileId: "file-made", ...over });
const artifact = { id: "artifact-made", sourceConceptRef: "file-made", title: "Notes.md" } as Row;
beforeEach(() => localStorage.clear());
function mount(seed: Parameters<typeof fakeConnection>[0] = {}, setup: Parameters<typeof composeIntent>[0] = { format: "markdown" }) {
 const conn=fakeConnection({materializeReply:{compositionId:"made"} as Row,...seed}); h.connection=conn;
 const tab={navigate:vi.fn(),close:vi.fn()},ports={reserve:vi.fn(()=>tab),navigate:vi.fn(),schedule:vi.fn(()=>()=>{})};
 const store=memoryStore(); let go!:(section:string)=>void; let sendIntent!:(intent:{id:string;payload:Record<string,unknown>})=>void; const navigate=vi.fn((section:string)=>go(section));
 function Harness(){const [section,setSection]=useState("composer");const [intent,setIntent]=useState({id:"setup",payload:composeIntent(setup)});go=setSection;sendIntent=setIntent;return withSession(<MaterializerApp sectionId={section} navigate={navigate} askContext={()=>{}} store={store} handoffPorts={ports} intent={intent} consumeIntent={()=>{}}/>);}
 render(<Harness/>); return {conn,tab,ports,navigate,go:(section:string)=>act(()=>go(section)),open:(id:string)=>act(()=>sendIntent({id:`open-${id}`,payload:{compositionId:id}}))};
}
async function name(value="Notes"){fireEvent.change(await screen.findByRole("textbox",{name:"Name"}),{target:{value}});}
async function describe(){fireEvent.click(screen.getByRole("button",{name:/^Describe/}));fireEvent.change(await screen.findByRole("textbox",{name:"Describe what you want"}),{target:{value:"Write a brief"}});}
async function make(){await readTheBar();fireEvent.click(await screen.findByRole("button",{name:"Make the file"}));}
async function making(){await screen.findByRole("button",{name:"Stop"});await readTheBar();}
const sent=(conn:ReturnType<typeof fakeConnection>,i=0)=>conn.query.composeMaterialize.mock.calls[i]?.[0];
it("starts one file on a double press and enters making before its row arrives",async()=>{const {conn}=mount();await name();const button=await screen.findByRole("button",{name:"Make the file"});await readTheBar();fireEvent.click(button);fireEvent.click(button);await making();expect(conn.query.composeMaterialize).toHaveBeenCalledOnce();expect(sent(conn)?.requestId).toMatch(/^[A-Za-z0-9_-]{8,111}$/);expect(screen.getByText("Starting",{selector:".os-actbar-detail"})).toBeTruthy();expect(screen.getByRole("button",{name:"Leave"})).toBeTruthy();expect(screen.queryByRole("button",{name:"Make the file"})).toBeNull();});
it("follows composing and rendering to the finished page",async()=>{const {conn,navigate}=mount();await name();await make();await making();act(()=>conn.subscriptions.emit(CONCEPT,ready({status:"composing"})));expect(await screen.findByText("Composing",{selector:".os-actbar-detail"})).toBeTruthy();act(()=>conn.subscriptions.emit(CONCEPT,ready({status:"rendering"})));expect(await screen.findByText("Writing the file",{selector:".os-actbar-detail"})).toBeTruthy();act(()=>conn.subscriptions.emit(CONCEPT,ready()));expect(await screen.findByRole("button",{name:"Open in editor"})).toBeTruthy();expect(navigate).toHaveBeenLastCalledWith("materialized");});
it("moves no window taken elsewhere",async()=>{const {conn,navigate,go}=mount();await name();await make();await making();go("settings");navigate.mockClear();act(()=>conn.subscriptions.emit(CONCEPT,ready()));await act(async()=>{});expect(navigate).not.toHaveBeenCalled();});
it("keeps an ask after refusal but renews it after an edit",async()=>{const {conn}=mount({writeError:new Error("That folder isn't available. Choose another folder.")},{format:"markdown",folderId:"missing"});await name();await make();expect(await screen.findByText("The file was not made.")).toBeTruthy();expect(conn.query.libraryFolderById).not.toHaveBeenCalled();expect(sent(conn)?.folderId).toBe("missing");await make();await waitFor(()=>expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));expect(sent(conn,1)?.requestId).toBe(sent(conn)?.requestId);await name("Other");await make();await waitFor(()=>expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(3));expect(sent(conn,2)?.requestId).not.toBe(sent(conn)?.requestId);});
it("Stop closes the tab and suppresses a late description",async()=>{const {conn,tab}=mount({cancelReply:{cancelled:false} as Row,outputArtifact:artifact});await name();await describe();await make();await making();fireEvent.click(await screen.findByRole("button",{name:"Stop"}));await waitFor(()=>expect(conn.query.composeCancel).toHaveBeenCalledWith({compositionId:"made",reason:"Stopped by the person"}));act(()=>conn.subscriptions.emit(CONCEPT,ready({statement:"Write a brief"})));expect(await screen.findByText("Stopped before the description was sent.",{selector:".os-notice-line"})).toBeTruthy();expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();expect(tab.close).toHaveBeenCalledOnce();expect(tab.navigate).not.toHaveBeenCalled();});
it("shows a Stop refusal and keeps the making stage",async()=>{const {conn}=mount();conn.query.composeCancel.mockRejectedValueOnce(refused("composeCancel","Try again in a moment."));await name();await make();await making();fireEvent.click(await screen.findByRole("button",{name:"Stop"}));expect(await screen.findByText("Try again in a moment.")).toBeTruthy();expect(screen.getByRole("button",{name:"Stop"})).toBeTruthy();});
it("opens a blank file once its Library entry arrives",async()=>{const {conn,tab,ports}=mount();await name();await make();await making();expect(ports.reserve).toHaveBeenCalledOnce();act(()=>conn.subscriptions.emit(CONCEPT,ready()));await waitFor(()=>expect(conn.query.libraryArtifactForFile).toHaveBeenCalled());expect(tab.navigate).not.toHaveBeenCalled();act(()=>conn.subscriptions.emit(Concepts.LIBRARY_ARTIFACT,artifact));await waitFor(()=>expect(tab.navigate).toHaveBeenCalledOnce());expect(tab.navigate.mock.calls[0]?.[0]).toContain("artifact-made");act(()=>conn.subscriptions.emit(CONCEPT,ready()));await act(async()=>{});expect(tab.navigate).toHaveBeenCalledOnce();});
it("opens a described file only after the revision reply, once",async()=>{const {conn,tab}=mount({outputArtifact:artifact});let complete!:()=>void;conn.query.libraryRequestDocumentRevision.mockImplementationOnce(()=>new Promise(resolve=>{complete=()=>resolve(rowsResult([{runId:"run-write"} as Row]));}));await name();await describe();await make();await making();act(()=>conn.subscriptions.emit(CONCEPT,ready({statement:"Write a brief"})));await waitFor(()=>expect(conn.query.libraryAddDocumentComment).toHaveBeenCalledOnce());expect(tab.navigate).not.toHaveBeenCalled();await act(async()=>complete());await waitFor(()=>expect(tab.navigate).toHaveBeenCalledOnce());expect(conn.query.libraryAddDocumentComment.mock.calls[0]?.[0]).toMatchObject({body:"Write a brief",artifactId:"artifact-made",requestId:`${sent(conn)?.requestId}-comment`});act(()=>conn.subscriptions.emit(CONCEPT,ready({statement:"Write a brief"})));await act(async()=>{});expect(conn.query.libraryAddDocumentComment).toHaveBeenCalledOnce();});
it("does not overwrite content when the server composed a predicted blank file",async()=>{const {conn,tab}=mount({outputArtifact:artifact});await name();await describe();await make();await making();act(()=>conn.subscriptions.emit(CONCEPT,ready({statement:"Write a brief",modelsUsed:[{provider:"vendor",model:"writer",calls:1,tokens:10}],provenanceEmbedded:true})));await screen.findByRole("button",{name:"Open in editor"});expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();expect(tab.close).toHaveBeenCalledOnce();});
it.each(["failed","cancelled"])("closes the tab when the file is %s",async status=>{const {conn,tab}=mount();await name();await make();await making();act(()=>conn.subscriptions.emit(CONCEPT,ready({status,outputFileId:"",failureReason:"No budget left"})));expect(await screen.findByRole("button",{name:"Start over"})).toBeTruthy();expect(tab.close).toHaveBeenCalledOnce();});
it("reserves nothing for an external editor",async()=>{localStorage.setItem("memql-os-editor-v1","vscode");const {ports}=mount();await name();await make();await making();expect(ports.reserve).not.toHaveBeenCalled();});
it("a CSV reference goes to the composer with its content",async()=>{const file=libraryFileRow({id:"csv",name:"figures.csv"});const {conn,ports}=mount({libraryFiles:[file]},{format:"markdown",sources:[{kind:"library_file",ref:"csv",label:"Figures"}]});await name();await describe();await make();await making();expect(sent(conn)?.sources).toEqual([{kind:"library_file",ref:"csv",label:"Figures",content:true}]);expect(ports.reserve).not.toHaveBeenCalled();act(()=>conn.subscriptions.emit(CONCEPT,ready({statement:"Write a brief"})));await act(async()=>{});expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();});

it("a pending ask abandoned through section navigation cannot keep the next file busy", async () => {
  const { conn, go } = mount();
  let finish!: () => void;
  conn.query.composeMaterialize.mockImplementationOnce(() => new Promise(resolve => { finish = () => resolve(rowsResult([{ compositionId: "old" } as Row])); }));
  await name("Old ask"); await make();
  await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
  go("settings");
  await act(async () => finish());
  go("composer");
  fireEvent.click(screen.getByRole("button", { name: /^Name/ }));
  await name("New ask"); await make();
  await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
  expect(sent(conn, 1)).toMatchObject({ name: "New ask" });
  expect(sent(conn, 1)?.requestId).not.toBe(sent(conn)?.requestId);
});
it("a recipe reply cannot take back a window moved to Settings", async () => {
  const { conn, go, navigate } = mount({ recipes: [{ id: "recipe", name: "Daily brief", format: "markdown", description: "Write the daily brief", status: "active", sources: [], createdAt: "2026-10-01T12:00:00Z" } as Row] });
  let finish!: () => void;
  conn.query.composeRunRecipe.mockImplementationOnce(() => new Promise(resolve => { finish = () => resolve(rowsResult([{ compositionId: "from-recipe" } as Row])); }));
  go("recipes");
  fireEvent.click(await screen.findByText("Daily brief"));
  await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Run it again" }));
  await waitFor(() => expect(conn.query.composeRunRecipe).toHaveBeenCalledOnce());
  go("settings"); navigate.mockClear();
  await act(async () => finish());
  expect(navigate).not.toHaveBeenCalled();
  expect(screen.getByRole("heading", { name: "Settings" })).toBeTruthy();
});

it("opening another running file keeps an older Make reply from replacing it", async () => {
  const { conn, open } = mount();
  let finish!: () => void;
  conn.query.composeMaterialize.mockImplementationOnce(() => new Promise(resolve => { finish = () => resolve(rowsResult([{ compositionId: "old" } as Row])); }));
  await name("Old ask"); await make();
  await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
  act(() => conn.subscriptions.emit(CONCEPT, ready({ id: "chosen", name: "Chosen file", status: "composing", outputFileId: "" })));
  open("chosen");
  await making();
  await act(async () => finish());
  await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  await waitFor(() => expect(conn.query.composeCancel).toHaveBeenCalledWith({ compositionId: "chosen", reason: "Stopped by the person" }));
});
