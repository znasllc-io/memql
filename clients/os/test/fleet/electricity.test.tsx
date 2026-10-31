import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { builtinReply, withSession, machineRow } from "./harness";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { chooseOption } from "../selectControl";
const h = vi.hoisted(() => ({ connection: null as unknown, open: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/ask/AskProvider", () => ({ useOpenAskWork: () => h.open }));
import { MachineElectricity } from "../../src/apps/fleet/machines/MachineElectricity";
const location = { address: "123 Private Fixture Lane", city: "Fixture City", region: "AZ", postalCode: "85000", country: "US", timezone: "America/Phoenix", verified: true, lookupAllowed: true, revision: "r1", locationRevision: "l1", watts: null, energySource: "", research: {}, tariff: {} };
const tariff = { provider: "Fixture utility", name: "Residential", currency: "USD", checkedAt: "2026-10-22T00:00:00Z", refreshAfter: "2026-11-22T00:00:00Z", effectiveFrom: "2026-01-01", sourceUrls: ["https://utility.example/rates"], rateBands: [{name:"Standard", perKWh:.1, appliesWhen:"All hours"}], assumptions: [] };
type Args = {action: string; revision?:string; secretSettings?:Record<string,unknown>; provider?:string; tariffName?:string};
function setup(initial: Record<string, unknown>, fail = false) {
 let saved = initial;
 const settings = vi.fn(async (args:Args) => {
  if(args.action === "read") return builtinReply("energy", [saved]);
  if(fail) throw new Error("Conflict: reload settings.");
  if(args.action === "save") saved={...saved,...args.secretSettings, revision:"r2",locationRevision:"l2"};
  return builtinReply("energy", [{revision:"r2",locationRevision:"l2"}]);
 });
 const create = vi.fn(async () => builtinReply("goal",[{runId:"utility-run"}]));
 h.connection={query:{fleetEnergySettings:settings, createGoal:create}};
 render(withSession(<MachineElectricity machine={machineFromRow(machineRow({id:"machine"}))}/>));
 return {settings,create};
}
afterEach(()=>{h.connection=null;h.open.mockClear();});
it("saves the private address but sends only locality into an automatically started research goal",async()=>{
 const {settings,create}=setup(location);
 const button=await screen.findByRole("button",{name:"Verify address and find utility"});
 await waitFor(()=>expect((button as HTMLButtonElement).disabled).toBe(false));
 await act(async()=>{fireEvent.click(button);});
 expect(settings.mock.calls.find(([a])=>a.action==="save")?.[0].secretSettings?.["address"]).toBe(location.address);
 expect(JSON.stringify(create.mock.calls)).not.toContain(location.address);
 expect(create).toHaveBeenCalledOnce(); expect(h.open).toHaveBeenCalledWith("utility-run");
});
it("retains edited energy values after a failed save and never starts research on failure",async()=>{
 const {create}=setup({...location,watts:100,energySource:"Owner estimate"},true);
 await waitFor(()=>expect((screen.getByLabelText("Estimated whole-machine watts") as HTMLInputElement).value).toBe("100"));
 fireEvent.change(screen.getByLabelText("Estimated whole-machine watts"),{target:{value:"250"}});
 await act(async()=>{fireEvent.click(screen.getByRole("button",{name:"Save energy estimate"}));});
 expect((screen.getByLabelText("Estimated whole-machine watts") as HTMLInputElement).value).toBe("250");
 expect(create).not.toHaveBeenCalled();
});
it("requires an explicit choice before confirming ambiguous findings",async()=>{
 const {settings}=setup({...location,research:{status:"ambiguous",tariffs:[tariff]}});
 const select=await screen.findByRole("combobox",{name:"Utility tariff"});
 const button=screen.getByRole("button",{name:"Confirm utility and tariff"});
 expect((button as HTMLButtonElement).disabled).toBe(true);
 chooseOption(select,"Fixture utility — Residential");
 await act(async()=>{fireEvent.click(button);});
 expect(settings.mock.calls.find(([a])=>a.action==="confirm")?.[0]).toMatchObject({provider:tariff.provider,tariffName:tariff.name,revision:"r1"});
});
