import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { rowsResult } from "../campaigns/harness";
import { configuredManifest, manifestYaml } from "../../src/apps/deployables/packages/manifest";
import type { PackageManifest } from "../../src/apps/deployables/packages/rows";
const h = vi.hoisted(() => ({connection: null as unknown}));
vi.mock("../../src/live/connection", () => ({useOsConnection: () => h.connection}));
import { CampaignPackageConfig } from "../../src/modules/connections/CampaignPackageConfig";
const manifest: PackageManifest = {formatVersion:1,name:"client-package",deployables:[{name:"web",path:"web",kind:"static",build:{command:"build",output:"out"},deployment:{slug:"quiet-cedar",domains:["www.example.com"]}}],campaigns:{azure:{subscriptionId:"subscription",resourceGroup:"mail",dataLocation:"United States"},domains:[{organization:"example.com",domain:"example.com",emailService:"email",communicationService:"delivery",sender:{username:"news",displayName:"Example"},dns:[{purpose:"Domain",name:"example.com",type:"TXT",value:"reference-only",ttl:3600}]}]}};
type Args = {action:string;manifest:string;accountId:string;organization:string;confirmed:boolean};
afterEach(() => {h.connection=null;vi.restoreAllMocks();});

it("loads installed choices without importing until the destination is reviewed", async () => {
 const call=vi.fn(async (args:Args)=>rowsResult([args.action==="installed" ? {status:"installed",manifest} : {status:"imported"}]));
 h.connection={query:{packageCampaigns:call}};const imported=vi.fn();
 render(<CampaignPackageConfig accountId="destination-client" organizationName="Client" onImported={imported}/>);
 await screen.findByText("client-package · memql-package.yaml");
 expect(call.mock.calls.map(([a])=>a.action)).toEqual(["installed"]);
 fireEvent.click(screen.getByText("Import email configuration"));
 expect(screen.getByText("Import for")).toBeTruthy();
 fireEvent.click(screen.getByRole("button",{name:"Import for Client"}));
 await waitFor(()=>expect(imported).toHaveBeenCalledOnce());
 expect(call.mock.calls[1]?.[0]).toEqual({action:"import",manifest:manifestYaml(manifest),accountId:"destination-client",organization:"example.com",confirmed:true});
 expect(await screen.findByText(/Configuration imported/)).toBeTruthy();
});

it("exports into the full loaded package and surfaces a refused operation", async () => {
 const call=vi.fn(async (args:Args)=>{if(args.action==="installed")return rowsResult([{status:"installed",manifest}]);throw Error("Azure connection needs sign-in");});
 h.connection={query:{packageCampaigns:call}};
 render(<CampaignPackageConfig accountId="client" organizationName="Client" onImported={()=>{}}/>);
 await screen.findByText("client-package · memql-package.yaml");
 fireEvent.click(screen.getByRole("button",{name:"Export configuration"}));
 expect(await screen.findByText("Package configuration could not be completed.")).toBeTruthy();
 expect(call.mock.calls[1]?.[0].manifest).toBe(manifestYaml(manifest));
 expect(call.mock.calls[1]?.[0].manifest).toContain('command: "build"');
});

it("discards a previous organization's late package response", async () => {
 let finish:(value:ReturnType<typeof rowsResult>)=>void=()=>{};
 const call=vi.fn((args:Args)=>args.accountId==="first" ? new Promise<ReturnType<typeof rowsResult>>(done=>{finish=done;}) : Promise.resolve(rowsResult([{status:"installed",manifest:null}])));
 h.connection={query:{packageCampaigns:call}};
 const view=render(<CampaignPackageConfig key="first" accountId="first" organizationName="First" onImported={()=>{}}/>);
 view.rerender(<CampaignPackageConfig key="second" accountId="second" organizationName="Second" onImported={()=>{}}/>);
 await act(async()=>{finish(rowsResult([{status:"installed",manifest}]));});
 expect(screen.queryByText("client-package · memql-package.yaml")).toBeNull();
});

it("retains campaign bindings when Deployables exports address choices", () => {
 const configured=configuredManifest(manifest, {web:{slug:"other-site",accountId:"client",ownDomain:"new.example.com"}},[],"cluster.example");
 expect(configured.campaigns).toEqual(manifest.campaigns);
 expect(configured.deployables[0]?.deployment).toEqual({slug:"other-site",domains:["new.example.com"]});
});

it("acknowledges the new capability only in Campaigns settings", async () => {
 const { AttentionDestination, AttentionProvider } = await import("../../src/attention/Attention");
 const { OS_REGISTRY } = await import("../../src/apps/registry");
 const { fakeConnection, withSession } = await import("../campaigns/harness");
 const conn=fakeConnection();
 const query=Object.assign(conn.query,{myAttentionReceipts:vi.fn(async()=>rowsResult([])),acknowledgeAttention:vi.fn(async()=>rowsResult([]))});
 h.connection={...conn,query:{...conn.query,packageCampaigns:vi.fn(async()=>rowsResult([{status:"installed",manifest}]))}};
 const app=OS_REGISTRY.apps.find(app=>app.id==="campaigns")!;
 const apps=[{...app,attentionChanges:app.attentionChanges!.filter(change=>change.id==="campaigns:package-config")}];
 const view=render(withSession(<AttentionProvider apps={apps}><AttentionDestination appId="campaigns" sectionId="overview"><span>Overview</span></AttentionDestination></AttentionProvider>));
 expect(query.acknowledgeAttention).not.toHaveBeenCalled();
 view.rerender(withSession(<AttentionProvider apps={apps}><AttentionDestination appId="campaigns" sectionId="settings"><CampaignPackageConfig accountId="client" organizationName="Client" onImported={()=>{}}/></AttentionDestination></AttentionProvider>));
 await screen.findByText("client-package · memql-package.yaml");
 await waitFor(()=>expect(query.acknowledgeAttention).toHaveBeenCalledWith({changeId:"campaigns:package-config",revision:"package-1"}));
});
