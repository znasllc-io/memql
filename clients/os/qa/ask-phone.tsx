import { createRoot } from "react-dom/client";
import { useEffect, useState } from "react";
import { AskProvider, useAsk } from "../src/ask/AskProvider";
import { PhoneShell } from "../src/chrome/PhoneShell";
import { AskSheet } from "../src/ask/AskSheet";
import { ConnectionNotice } from "../src/kit/ConnectionNotice";
import { ConnectionRecoveryContext, ConnectionStatusContext, type ShellConnectionStatus } from "../src/chrome/connection";
import { OsProvider } from "../src/chrome/state";
import { OS_REGISTRY } from "../src/apps/registry";
import { READY_ASK } from "../src/ask/useAskReadiness";
import type { AskTurn } from "../src/ask/conversationSession";
import { withSession } from "../test/cluster/harness";
import { coreAt } from "../test/setup/harness";
import { installSeededAccess } from "../test/seededAccess";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Real phone chrome and Ask over synthetic receipts; no network or model calls.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]); installSeededAccess("owner");
const paused = params.get("scene") !== "empty";
const desktop = params.get("layout") === "desktop";
const turns: AskTurn[] = paused ? [{id:"turn",runId:"fixture-run",workTitle:"Create the dog image",prompt:"Create an image of a black cocker spaniel.",answer:"I’ll create and save the image.",acknowledgement:"I’ll create and save the image.",background:true,state:"waiting",workStatus:"waiting",startedAt:"2026-10-18T13:00:00Z",activity:[]}] : [];
const transport = {ask(){throw new Error("This preview does not execute work.");},conversations:{list:async()=>[{id:"fixture",title:"Dog image"}],create:async()=>({id:"new",title:"New conversation"}),read:async()=>turns}};
function Open() {
 const {conversation,openAsk} = useAsk();
 const [status,setStatus] = useState<ShellConnectionStatus>(params.has("reconnect") ? "reconnecting" : "connected");
 useEffect(() => { let active=true; void conversation.select("fixture").then(() => {if(active)openAsk();});return()=>{active=false;};},[conversation,openAsk]);
 return <ConnectionRecoveryContext.Provider value={{managed:true,retry:()=>setStatus("connected")}}><ConnectionStatusContext.Provider value={status}>
   {desktop ? <><AskSheet /><ConnectionNotice className="os-connection-floating" /></> : <PhoneShell onSignOut={()=>{document.title="Sign out selected in fixture";}} />}
 </ConnectionStatusContext.Provider></ConnectionRecoveryContext.Provider>;
}
const source = new URL(location.href); source.searchParams.delete("frame");
createRoot(document.getElementById("root")!).render(params.has("frame")
 ? <iframe title="Mobile Ask preview" src={source.toString()} style={{width:params.get("frame")==="small"?320:390,height:params.get("frame")==="small"?568:844,border:0}} />
 : withSession(<OsProvider registry={OS_REGISTRY} actorRole="owner" grid={{cols:12,rows:8}} layout={desktop ? "desktop" : "phone"}><AskProvider transport={transport} availability={READY_ASK}><div className="os-root" data-layout={desktop ? "desktop" : "phone"}><Open /></div></AskProvider></OsProvider>,{readiness:coreAt("configured","configured","configured")}));
